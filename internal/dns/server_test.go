package dns

import (
	"testing"
	"time"

	mdns "github.com/miekg/dns"
)

func TestEmbeddedDNSResolutionAndUpdate(t *testing.T) {
	srv, err := StartServer("127.0.0.1:0", "shop", "")
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	defer srv.Close()

	if err := srv.SetRecord("db", "10.88.1.10"); err != nil {
		t.Fatal(err)
	}
	if err := srv.SetRecord("api", "10.88.1.11"); err != nil {
		t.Fatal(err)
	}

	client := &mdns.Client{Timeout: 2 * time.Second}

	query := func(name string) string {
		m := new(mdns.Msg)
		m.SetQuestion(mdns.Fqdn(name), mdns.TypeA)
		resp, _, err := client.Exchange(m, srv.Addr())
		if err != nil {
			t.Fatalf("Exchange %s: %v", name, err)
		}
		if len(resp.Answer) != 1 {
			t.Fatalf("expected 1 A record for %s, got %d", name, len(resp.Answer))
		}
		return resp.Answer[0].(*mdns.A).A.String()
	}

	if got := query("db"); got != "10.88.1.10" {
		t.Fatalf("expected 10.88.1.10, got %s", got)
	}
	if got := query("api.shop.yak"); got != "10.88.1.11" {
		t.Fatalf("expected 10.88.1.11, got %s", got)
	}

	// Simulate blue/green cutover of api IP.
	if err := srv.SetRecord("api", "10.88.1.12"); err != nil {
		t.Fatal(err)
	}
	if got := query("api"); got != "10.88.1.12" {
		t.Fatalf("expected updated IP 10.88.1.12, got %s", got)
	}
}
