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
