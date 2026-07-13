package cli

import "testing"

func TestParseConnectorPortChecksNetworkRange(t *testing.T) {
	for _, value := range []string{"", "0", "65536", "not-a-port"} {
		if _, err := parseConnectorPort(value, "MySQL"); err == nil {
			t.Fatalf("expected port %q to fail", value)
		}
	}
	port, err := parseConnectorPort("5432", "PostgreSQL")
	if err != nil || port != 5432 {
		t.Fatalf("valid port: port=%d err=%v", port, err)
	}
}
