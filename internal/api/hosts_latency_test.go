package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/hosts"
)

// slowHost answers discovery and resource listings only once release is
// closed, like a host whose SSH connection hangs.
type slowHost struct {
	stub
	release chan struct{}
}

func (s *slowHost) Unmanaged(ctx context.Context, host string) (*controller.Unmanaged, error) {
	<-s.release
	return s.stub.Unmanaged(ctx, host)
}

func (s *slowHost) HostResources(ctx context.Context, host string) (*hosts.Resources, error) {
	<-s.release
	return s.stub.HostResources(ctx, host)
}

// Host pages render from what is cached, so a host that is slow to scan
// doesn't hold them up: the page says it is scanning, and the scan's result
// arrives over SSE.
func TestHostPagesDontWaitOnSlowHosts(t *testing.T) {
	a := newAuthn(t, "secret")
	st := &slowHost{release: make(chan struct{})}
	srv := httptest.NewServer(New(st, Options{Auth: a}))
	defer srv.Close()
	var once sync.Once
	release := func() { once.Do(func() { close(st.release) }) }
	defer release()
	for _, path := range []string{"/hosts", "/hosts/nas", "/hosts", "/hosts/nas"} {
		start := time.Now()
		code, body := do(t, srv, "GET", path, "secret", "")
		took := time.Since(start)
		t.Logf("%s took %s", path, took)
		if code != 200 || !strings.Contains(body, "Scanning") {
			t.Fatalf("%s: %d %s", path, code, body)
		}
		if took > 500*time.Millisecond {
			t.Fatalf("%s took %s waiting on a slow host", path, took)
		}
	}
	// An open page fills in over SSE when the scan finishes.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events?page=/hosts/nas", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	next := func() string {
		var frame strings.Builder
		for sc.Scan() {
			if sc.Text() == "" {
				return frame.String()
			}
			frame.WriteString(sc.Text() + "\n")
		}
		t.Fatal("missing frame", sc.Err())
		return ""
	}
	if frame := next(); !strings.Contains(frame, "Scanning") {
		t.Fatalf("first frame isn't scanning: %s", frame)
	}
	release()
	for {
		frame := next()
		if strings.Contains(frame, `href="/hosts/nas/adopt/external"`) && strings.Contains(frame, "traefik-proxy-1") {
			break
		}
	}
}

// eventually repeats get until its body contains want, as a host page does once
// the background scan its first load started finishes, and returns the last try.
func eventually(t *testing.T, want string, get func() (int, string)) (int, string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := get()
		if (code == 200 && strings.Contains(body, want)) || time.Now().After(deadline) {
			return code, body
		}
		time.Sleep(5 * time.Millisecond)
	}
}
