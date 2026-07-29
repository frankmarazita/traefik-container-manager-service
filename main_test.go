package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	containertypes "github.com/docker/docker/api/types/container"
)

func labelled(labels map[string]string) containertypes.Summary {
	return containertypes.Summary{Labels: labels}
}

func TestPrefixMatch(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		label     string
		want      bool
	}{
		{"empty requested never matches", "", "app.example.com", false},
		{"empty label never matches", "app.example.com", "", false},
		{"both empty never matches", "", "", false},
		{"exact match", "app.example.com", "app.example.com", true},
		{"requested extends label", "app.example.com/health", "app.example.com", true},
		{"label extends requested", "app.example.com", "app.example.com/health", true},
		{"unrelated values", "other.example.com", "app.example.com", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := prefixMatch(tc.requested, tc.label); got != tc.want {
				t.Errorf("prefixMatch(%q, %q) = %v, want %v", tc.requested, tc.label, got, tc.want)
			}
		})
	}
}

func TestServiceMatchesIgnoresForeignContainers(t *testing.T) {
	service := &Service{name: "myapp"}
	foreign := labelled(map[string]string{
		"traefik-container-manager.name": "other",
		"traefik-container-manager.host": "other.example.com",
		"traefik-container-manager.path": "/other",
	})

	if service.matches(foreign) {
		t.Error("service with no host or path must not match a foreign container's host/path labels")
	}
}

func TestServiceMatchesByName(t *testing.T) {
	service := &Service{name: "myapp"}
	own := labelled(map[string]string{"traefik-container-manager.name": "MyApp"})

	if !service.matches(own) {
		t.Error("expected case-insensitive match on the name label")
	}
}

func TestServiceMatchesFallsThroughToPath(t *testing.T) {
	service := &Service{name: "generic", host: "wrong.example.com", path: "/app"}
	container := labelled(map[string]string{
		"traefik-container-manager.name": "other",
		"traefik-container-manager.host": "app.example.com",
		"traefik-container-manager.path": "/app",
	})

	if !service.matches(container) {
		t.Error("a non-matching host label must not stop the path label from being checked")
	}
}

func TestServiceMatchesEmptyNameLabel(t *testing.T) {
	service := &Service{}
	container := labelled(map[string]string{"traefik-container-manager.name": ""})

	if service.matches(container) {
		t.Error("an empty service name must not match an empty name label")
	}
}

func TestGetOrCreateServiceIsRaceFree(t *testing.T) {
	servicesMu.Lock()
	services = map[string]*Service{}
	servicesMu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if _, err := GetOrCreateService(fmt.Sprintf("svc-%d", n%5), 60, "", "", nil); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	servicesMu.Lock()
	defer servicesMu.Unlock()
	if len(services) != 5 {
		t.Errorf("expected 5 distinct services, got %d", len(services))
	}
}

func TestGetOrCreateServiceReturnsSameInstance(t *testing.T) {
	servicesMu.Lock()
	services = map[string]*Service{}
	servicesMu.Unlock()

	first, err := GetOrCreateService("dupe", 60, "", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := GetOrCreateService("dupe", 120, "", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first != second {
		t.Error("expected the cached service instance to be reused")
	}
}

func TestClaimAdmitsOneGoroutine(t *testing.T) {
	service := &Service{name: "claimed", time: make(chan uint64, 1)}

	var claims int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			service.mu.Lock()
			defer service.mu.Unlock()
			if service.claimLocked() {
				atomic.AddInt64(&claims, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&claims); got != 1 {
		t.Errorf("expected exactly 1 claim, got %d", got)
	}

	service.releaseHandler()
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.claimLocked() {
		t.Error("expected the handler to be claimable again after release")
	}
}

func TestDrainOrStopExtendsOnKeepalive(t *testing.T) {
	service := &Service{name: "svc", time: make(chan uint64, 1)}
	service.time <- 42

	stopped := false
	extended, timeout := service.drainOrStop(func() { stopped = true })

	if !extended {
		t.Error("expected a pending keepalive to extend the timeout")
	}
	if timeout != 42 {
		t.Errorf("expected the queued timeout 42, got %d", timeout)
	}
	if stopped {
		t.Error("the service must not be stopped when a keepalive is pending")
	}
}

func TestDrainOrStopReleasesHandlerOnlyAfterStopping(t *testing.T) {
	service := &Service{name: "svc", time: make(chan uint64, 1)}
	service.isHandled = true

	stopped := false
	extended, _ := service.drainOrStop(func() {
		stopped = true
		if !service.isHandled {
			t.Error("handler was released before the stop completed")
		}
	})

	if extended {
		t.Error("expected the service to stop when no keepalive is pending")
	}
	if !stopped {
		t.Error("expected the stop function to run")
	}

	service.mu.Lock()
	defer service.mu.Unlock()
	if service.isHandled {
		t.Error("expected the handler to be released once the service is stopped")
	}
}

func TestDrainOrStopHoldsLockWhileStopping(t *testing.T) {
	service := &Service{name: "svc", time: make(chan uint64, 1)}
	acquired := make(chan struct{})

	service.drainOrStop(func() {
		go func() {
			service.mu.Lock()
			service.mu.Unlock()
			close(acquired)
		}()
		select {
		case <-acquired:
			t.Error("a request observed the service while it was being stopped")
		case <-time.After(100 * time.Millisecond):
		}
	})

	<-acquired
}

func parseQuery(t *testing.T, query string) (string, uint64, string, string, error) {
	t.Helper()
	return parseParams(httptest.NewRequest(http.MethodGet, "/api/?"+query, nil))
}

func TestParseParamsRejectsNegativeTimeout(t *testing.T) {
	_, timeout, _, _, err := parseQuery(t, "name=myapp&timeout=-1")
	if err == nil {
		t.Fatal("expected a negative timeout to be rejected")
	}
	if timeout != 0 {
		t.Errorf("expected timeout 0 on rejection, got %d", timeout)
	}
}

func TestNegativeTimeoutNoLongerInvertsSleep(t *testing.T) {
	parsed := -1
	if d := time.Duration(uint64(parsed)) * time.Second; d >= 0 {
		t.Skip("platform does not reproduce the original wraparound")
	}
	if _, _, _, _, err := parseQuery(t, "name=myapp&timeout=-1"); err == nil {
		t.Error("a timeout that wraps to a negative duration must not be accepted")
	}
}

func TestParseParamsRejectsOversizedTimeout(t *testing.T) {
	_, _, _, _, err := parseQuery(t, fmt.Sprintf("name=myapp&timeout=%d", maxTimeout+1))
	if err == nil {
		t.Fatal("expected an oversized timeout to be rejected")
	}
}

func TestParseParamsAcceptsMaxTimeout(t *testing.T) {
	_, timeout, _, _, err := parseQuery(t, fmt.Sprintf("name=myapp&timeout=%d", maxTimeout))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d := time.Duration(timeout) * time.Second; d <= 0 {
		t.Errorf("maxTimeout must stay positive as a Duration, got %v", d)
	}
}

func TestParseParamsRejectsNonNumericTimeout(t *testing.T) {
	if _, _, _, _, err := parseQuery(t, "name=myapp&timeout=abc"); err == nil {
		t.Error("expected a non-numeric timeout to be rejected")
	}
}

func TestParseParamsReadsValidRequest(t *testing.T) {
	name, timeout, host, path, err := parseQuery(t, "name=myapp&timeout=60&host=app.example.com&path=%2Fhealth")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "myapp" || timeout != 60 || host != "app.example.com" || path != "/health" {
		t.Errorf("got name=%q timeout=%d host=%q path=%q", name, timeout, host, path)
	}
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		states []string
		want   Status
	}{
		{"all running is up", []string{"running", "running"}, UP},
		{"any restarting is starting", []string{"running", "restarting"}, STARTING},
		{"created needs starting", []string{"created"}, DOWN},
		{"exited needs starting", []string{"running", "exited"}, DOWN},
		{"dead needs starting", []string{"dead"}, DOWN},
		{"no containers is unknown", []string{}, UNKNOWN},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			containers := make([]containertypes.Summary, 0, len(tc.states))
			for _, state := range tc.states {
				containers = append(containers, containertypes.Summary{State: state})
			}
			if got := statusOf(containers); got != tc.want {
				t.Errorf("states %v gave %q, want %q", tc.states, got, tc.want)
			}
		})
	}
}
