package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// The login URL is the one line of tsnet output a human must see, and it arrives on the
// same callback as everything else tsnet says. At the default level, Debug is dropped —
// so routing the whole callback there would make a first run look like silence.
func TestTsnetLogfSurfacesTheLoginURL(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{
			name: "login url",
			msg:  "To authenticate, visit:\n\n\thttps://login.tailscale.com/a/0123456789ab\n\n",
			want: true,
		},
		{
			name: "ordinary chatter",
			msg:  "magicsock: disco: node [abcd] d:1234 now using 192.168.0.5:41641",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			// Deliberately the default level, which is what the service runs at.
			log := slog.New(slog.NewJSONHandler(&buf, nil))
			tsnetLogf(log)("%s", tt.msg)

			got := buf.Len() > 0
			if got != tt.want {
				t.Fatalf("logged = %v, want %v (output: %q)", got, tt.want, buf.String())
			}
			if tt.want && !strings.Contains(buf.String(), "login.tailscale.com") {
				t.Errorf("the URL itself was not logged: %s", buf.String())
			}
		})
	}
}

// The callback takes printf arguments; a stray percent sign in tsnet output must not
// turn into a formatting artefact.
func TestTsnetLogfFormats(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	tsnetLogf(log)("To authenticate, visit: %s (%d%% done)", "https://login.tailscale.com/a/xyz", 50)

	out := buf.String()
	if !strings.Contains(out, "https://login.tailscale.com/a/xyz") || !strings.Contains(out, "50%") {
		t.Errorf("unexpected output: %s", out)
	}
}
