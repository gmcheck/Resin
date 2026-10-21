package outbound

import (
	"strings"
	"testing"
)

func TestRedactURLCredentials(t *testing.T) {
	cases := []struct{ in, want string }{
		{`parse "socks5://user:secret@host:bad": invalid port`, `parse "socks5://user:***@host:bad": invalid port`},
		{`socks5://host:1080`, `socks5://host:1080`},
		{`http://user@host:8080/path`, `http://user@host:8080/path`},
		{`no url here`, `no url here`},
		{`a://u:p@h b://u2:p2@h2`, `a://u:***@h b://u2:***@h2`},
	}
	for _, c := range cases {
		if got := redactURLCredentials(c.in); got != c.want {
			t.Errorf("redactURLCredentials(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseUpstreamProxyURL_ErrorDoesNotLeakPassword(t *testing.T) {
	const secret = "sup3rs3cret"
	_, err := parseUpstreamProxyURL("socks5://user:" + secret + "@host:notaport")
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks the password: %v", err)
	}
}
