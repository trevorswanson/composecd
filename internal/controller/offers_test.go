package controller

import (
	"errors"
	"strings"
	"testing"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/store"
)

func TestImageOfferWaitsForApproval(t *testing.T) {
	e, reg := newStrategyEnv(t, "3.0.1", "offer: {}", "3.0.1", "3.1.0", "4.0.0-rc.1", "latest")
	reg.set(nginxRepo+":3.1.0", digestB)
	n := &recorder{}
	e.c.Notify = n
	e.refresh()
	if !strings.Contains(e.fake().Running["web"], "image: nginx:3.0.1@"+digestA) {
		t.Fatalf("first sync:\n%s", e.fake().Running["web"])
	}

	// A check finds the newer tag and reports it, and nothing deploys it.
	e.checkImages("web")
	e.refresh()
	web := e.app("web")
	im := imageOf(web, "web")
	if im == nil || im.Strategy != "digest" || im.UpdateAvailable || im.Offer == nil || im.Offer.Tag != "3.1.0" || im.Offer.Digest != digestB ||
		!im.Offer.Upgradable || im.Offer.Constraint != "*" || im.Offer.Skipped {
		t.Fatalf("image = %+v, offer %+v", im, im.Offer)
	}
	if web.Sync.Status != Synced || !strings.Contains(e.fake().Running["web"], "image: nginx:3.0.1@"+digestA) {
		t.Fatalf("the offer deployed: %+v\n%s", web.Sync, e.fake().Running["web"])
	}
	if !n.has(config.EventImageAvailable, "web: nginx (web): 3.1.0 available, beyond 3.0.1; nothing deploys it until an admin approves the upgrade") {
		t.Errorf("events = %+v", n.events)
	}

	// A new digest for Git's tag still deploys on its own, and the offer
	// isn't sent again.
	reg.set(nginxRepo+":3.0.1", digestC)
	e.checkImages("web")
	e.c.ops.Wait()
	e.refresh()
	if !strings.Contains(e.fake().Running["web"], "image: nginx:3.0.1@"+digestC) {
		t.Fatalf("the digest update didn't deploy:\n%s", e.fake().Running["web"])
	}
	sent := 0
	for _, ev := range n.events {
		if ev.Type == config.EventImageAvailable {
			sent++
		}
	}
	if sent != 1 {
		t.Errorf("image-available sent %d times", sent)
	}

	// Deciding another tag than the one offered is refused.
	if _, err := e.c.DecideUpgrade("web", "web", store.UpgradeApprove, "3.0.9", "trevor"); !errors.Is(err, ErrOfferChanged) {
		t.Errorf("approving a stale tag: %v", err)
	}

	// A skipped tag stays skipped until a newer one.
	if _, err := e.c.DecideUpgrade("web", "web", store.UpgradeSkip, "3.1.0", "trevor"); err != nil {
		t.Fatal(err)
	}
	e.c.ops.Wait()
	if im := imageOf(e.app("web"), "web"); im == nil || im.Offer == nil || !im.Offer.Skipped || im.Offer.SkippedBy != "trevor" {
		t.Fatalf("after skip: %+v", im)
	}
	reg.setTags(nginxRepo, digestB, "3.0.1", "3.1.0", "3.2.0")
	reg.set(nginxRepo+":3.0.1", digestC)
	e.checkImages("web")
	if im := imageOf(e.app("web"), "web"); im == nil || im.Offer == nil || im.Offer.Tag != "3.2.0" || im.Offer.Skipped {
		t.Fatalf("after a newer tag: %+v", im)
	}

	// An approved upgrade deploys the tag as a strategy's pick would.
	if _, err := e.c.DecideUpgrade("web", "web", store.UpgradeApprove, "3.2.0", "trevor"); err != nil {
		t.Fatal(err)
	}
	e.c.ops.Wait()
	e.refresh()
	web = e.app("web")
	im = imageOf(web, "web")
	if !strings.Contains(e.fake().Running["web"], "image: nginx:3.2.0@"+digestB) || web.Sync.Status != Synced {
		t.Fatalf("approved upgrade not deployed: %+v\n%s", web.Sync, e.fake().Running["web"])
	}
	if im == nil || im.Approved != "3.2.0" || im.ApprovedBy != "trevor" || im.Strategy != "digest" || im.Offer != nil || im.Tag != "3.2.0" {
		t.Errorf("image after upgrade = %+v", im)
	}
	ops, err := e.c.Store.Operations("web", 10)
	if err != nil || !slicesContain(ops, func(o *store.Operation) bool {
		return o.Kind == store.KindImageDecision && o.User == "trevor" && strings.Contains(o.Message, "approved the upgrade from 3.0.1 to 3.2.0")
	}) {
		t.Errorf("operations = %v, %v", ops, err)
	}

	// Once Git renders another image, the approval lapses.
	e.commit(map[string]string{"charts/web/values.yaml": "tag: '3.2.0'\n"})
	e.refresh()
	if ups, _ := e.c.Store.ImageUpgrades("web"); len(ups) != 0 {
		t.Errorf("upgrades after Git moved = %+v", ups)
	}
	if im := imageOf(e.app("web"), "web"); im == nil || im.Approved != "" || im.Strategy != "digest" || im.Candidate != "" {
		t.Errorf("image after Git moved = %+v", im)
	}
}

func TestImageOfferBeyondTheStrategy(t *testing.T) {
	e, _ := newStrategyEnv(t, "1.27.1", `strategy: semver, constraint: "~1.27", offer: {constraint: ">=1.28", ignoreTags: ["2.*"]}`,
		"1.27.1", "1.27.3", "1.28.0", "2.0.0")
	e.refresh()
	e.checkImages("web")
	im := imageOf(e.app("web"), "web")
	if !strings.Contains(e.fake().Running["web"], "image: nginx:1.27.3@") || im == nil || im.Candidate != "1.27.3" || im.Offer == nil || im.Offer.Tag != "1.28.0" {
		t.Fatalf("image = %+v\n%s", im, e.fake().Running["web"])
	}
}

func TestImageOfferGitPinned(t *testing.T) {
	t.Run("write-back commits the approved upgrade", func(t *testing.T) {
		values := "tag: '3.0.1@" + digestA + "'   # pinned\n"
		e, reg := newWriteBackEnv(t, map[string]string{"charts/web/values.yaml": values,
			"composecd/apps.yaml": strategyApp("web", `writeBack: git, offer: {constraint: ">=3"}`)}, "3.0.1", "3.1.0")
		reg.set(nginxRepo+":3.1.0", digestB)
		e.refresh()
		e.checkImages("web")
		e.refresh()
		head := e.git("rev-parse", "HEAD")
		im := imageOf(e.app("web"), "web")
		if im == nil || im.Pin != PinGit || im.UpdateAvailable || im.Offer == nil || im.Offer.Tag != "3.1.0" || !im.Offer.Upgradable ||
			im.Offer.WriteBack != config.WriteBackGit {
			t.Fatalf("image = %+v", im)
		}
		if got := e.show("charts/web/values.yaml"); got != values {
			t.Fatalf("the offer was committed unapproved:\n%s", got)
		}

		if _, err := e.c.DecideUpgrade("web", "web", store.UpgradeApprove, "", "trevor"); err != nil {
			t.Fatal(err)
		}
		e.c.ops.Wait()
		e.refresh()
		if got, want := e.show("charts/web/values.yaml"), "tag: '3.1.0@"+digestB+"'   # pinned\n"; got != want {
			t.Errorf("values.yaml = %q, want %q", got, want)
		}
		if e.git("rev-parse", "HEAD~1") != head {
			t.Error("the upgrade isn't one commit on the deployed branch")
		}
		web := e.app("web")
		if !strings.Contains(e.fake().Running["web"], "image: nginx:3.1.0@"+digestB+"\n") || web.Sync.Status != Synced {
			t.Fatalf("not deployed from Git: %+v\n%s", web.Sync, e.fake().Running["web"])
		}
		if ups, _ := e.c.Store.ImageUpgrades("web"); len(ups) != 0 {
			t.Errorf("upgrades after the commit = %+v", ups)
		}
		if im := imageOf(web, "web"); im == nil || im.Offer != nil || im.Approved != "" || im.UpdateAvailable {
			t.Errorf("image after the commit = %+v", im)
		}
	})

	t.Run("a moving tag's approved upgrade is written back too", func(t *testing.T) {
		e, reg := newWriteBackEnv(t, map[string]string{"charts/web/values.yaml": "tag: '3.0.1'\n",
			"composecd/apps.yaml": strategyApp("web", "writeBack: git, offer: {}")}, "3.0.1", "3.1.0")
		reg.set(nginxRepo+":3.1.0", digestB)
		e.refresh()
		e.checkImages("web")
		if _, err := e.c.DecideUpgrade("web", "web", store.UpgradeApprove, "3.1.0", "trevor"); err != nil {
			t.Fatal(err)
		}
		e.c.ops.Wait()
		e.refresh()
		if got := e.show("charts/web/values.yaml"); got != "tag: '3.1.0'\n" {
			t.Errorf("values.yaml = %q", got)
		}
		if !strings.Contains(e.fake().Running["web"], "image: nginx:3.1.0@"+digestB) {
			t.Fatalf("not deployed:\n%s", e.fake().Running["web"])
		}
		if im := imageOf(e.app("web"), "web"); im == nil || im.Diverged || im.Approved != "" || im.Strategy != "digest" {
			t.Errorf("image = %+v", im)
		}
	})

	t.Run("without write-back it is only reported", func(t *testing.T) {
		e, reg := newStrategyEnv(t, "3.0.1@"+digestA, "offer: {}", "3.0.1", "3.1.0")
		reg.set(nginxRepo+":3.1.0", digestB)
		e.refresh()
		e.checkImages("web")
		im := imageOf(e.app("web"), "web")
		if im == nil || im.Offer == nil || im.Offer.Upgradable || im.Offer.Note != offerPinnedNote {
			t.Fatalf("image = %+v", im)
		}
		if _, err := e.c.DecideUpgrade("web", "web", store.UpgradeApprove, "3.1.0", "trevor"); !errors.Is(err, ErrNotUpgradable) {
			t.Errorf("approving: %v", err)
		}
		if _, err := e.c.DecideUpgrade("web", "web", store.UpgradeSkip, "3.1.0", "trevor"); err != nil {
			t.Errorf("skipping: %v", err)
		}
	})
}

func TestImageNoOffer(t *testing.T) {
	e, _ := newStrategyEnv(t, "3.0.1", "strategy: digest", "3.0.1", "3.1.0")
	e.refresh()
	e.checkImages("web")
	if im := imageOf(e.app("web"), "web"); im == nil || im.Offer != nil {
		t.Fatalf("image = %+v", im)
	}
	if _, err := e.c.DecideUpgrade("web", "web", store.UpgradeApprove, "", "trevor"); !errors.Is(err, ErrNoOffer) {
		t.Errorf("approving: %v", err)
	}
}
