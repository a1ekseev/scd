package inputsrc

import "testing"

func TestRedactInput(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"https path and query", "https://sub.example.test/api/sub/AbC123tok?token=s3cr3t#frag", "https://sub.example.test/<redacted>"},
		{"http with port and userinfo", "HTTP://user:pw@h.example.test:8443/x", "http://h.example.test:8443/<redacted>"},
		{"origin only", "https://h.example.test", "https://h.example.test/<redacted>"},
		{"unparsable url", "https://h.example.test/%zz", "<redacted>"},
		{"file path unchanged", "/app/fixtures/subscription.txt", "/app/fixtures/subscription.txt"},
		{"stdin unchanged", "-", "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactInput(tt.in); got != tt.want {
				t.Errorf("RedactInput(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRedactInputIn(t *testing.T) {
	url := "https://sub.example.test/api/sub/AbC123tok?token=s3cr3t"
	msg := "unable to download input from " + url + ": 404 Not Found"
	want := "unable to download input from https://sub.example.test/<redacted>: 404 Not Found"
	if got := RedactInputIn(msg, url); got != want {
		t.Errorf("RedactInputIn = %q, want %q", got, want)
	}
	if got := RedactInputIn("plain text", "/a/b"); got != "plain text" {
		t.Errorf("RedactInputIn for a file path = %q", got)
	}
	if got := RedactInputIn("x", ""); got != "x" {
		t.Errorf("RedactInputIn with empty input = %q", got)
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"port kept as written", "https://h:8443/x?t=1", "https://h:8443/<redacted>"},
		{"default port kept", "http://h.example.test:80/a", "http://h.example.test:80/<redacted>"},
		{"any scheme", "socks5://127.0.0.1:1080/p", "socks5://127.0.0.1:1080/<redacted>"},
		{"userinfo dropped", "https://u:p@h.example.test/a", "https://h.example.test/<redacted>"},
		{"not a url", "not a url", "<redacted>"},
		{"no host", "mailto:x@example.test", "<redacted>"},
		{"unparsable", "https://h.example.test/%zz", "<redacted>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactURL(tt.in); got != tt.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
