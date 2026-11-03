package config

import (
	"strings"
	"testing"
	"time"
)

func hkHost(housekeeping string) string {
	return "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: nas}\nspec:\n  housekeeping:\n" + housekeeping
}

func TestHousekeepingFields(t *testing.T) {
	objs, err := Parse("t.yaml", []byte(hkHost(`    imagePrune: {schedule: "0 4 * * 0", timeZone: Europe/Berlin, until: 168h, all: true}
    builderPrune: {schedule: "@weekly", keepStorage: 10GB}
    diskWarning: {percent: 85}
`)))
	if err != nil {
		t.Fatal(err)
	}
	hk := objs.Hosts[0].Spec.Housekeeping
	if p := hk.ImagePrune; p.Schedule != "0 4 * * 0" || p.TimeZone != "Europe/Berlin" || time.Duration(p.Until) != 168*time.Hour || !p.All {
		t.Errorf("imagePrune = %+v", p)
	}
	if p := hk.BuilderPrune; p.Schedule != "@weekly" || p.KeepStorage != "10GB" {
		t.Errorf("builderPrune = %+v", p)
	}
	if hk.DiskWarning.Percent != 85 {
		t.Errorf("diskWarning = %+v", hk.DiskWarning)
	}
	// An empty section only measures the disk.
	if objs, err := Parse("t.yaml", []byte(hkHost("    {}\n"))); err != nil || objs.Hosts[0].Spec.Housekeeping == nil {
		t.Errorf("empty housekeeping: %v", err)
	}
}

func TestHousekeepingValidation(t *testing.T) {
	cases := map[string]string{
		"schedule":     `    imagePrune: {schedule: "weekly-ish"}`,
		"time zone":    `    builderPrune: {schedule: "@daily", timeZone: Mars/Base}`,
		"negative":     `    imagePrune: {until: -1h}`,
		"keep storage": `    builderPrune: {keepStorage: lots}`,
		"percent zero": `    diskWarning: {percent: 0}`,
		"percent big":  `    diskWarning: {percent: 101}`,
		"unknown":      `    volumePrune: {schedule: "@daily"}`,
	}
	for name, hk := range cases {
		if _, err := Parse("t.yaml", []byte(hkHost(hk+"\n"))); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if name == "keep storage" && !strings.Contains(err.Error(), "keepStorage") {
			t.Errorf("%s: error = %v", name, err)
		}
	}
	for _, size := range []string{"10GB", "512MiB", "1.5g", "1024", "100kb", "3 GB"} {
		if !dockerSize.MatchString(size) {
			t.Errorf("size %q rejected", size)
		}
	}
}
