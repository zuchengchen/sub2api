package urlvalidator

import "testing"

func TestValidateURLFormat(t *testing.T) {
	if _, err := ValidateURLFormat("", false); err == nil {
		t.Fatalf("expected empty url to fail")
	}
	if _, err := ValidateURLFormat("://bad", false); err == nil {
		t.Fatalf("expected invalid url to fail")
	}
	if _, err := ValidateURLFormat("http://example.com", false); err == nil {
		t.Fatalf("expected http to fail when allow_insecure_http is false")
	}
	if _, err := ValidateURLFormat("https://example.com", false); err != nil {
		t.Fatalf("expected https to pass, got %v", err)
	}
	if _, err := ValidateURLFormat("http://example.com", true); err != nil {
		t.Fatalf("expected http to pass when allow_insecure_http is true, got %v", err)
	}
	if _, err := ValidateURLFormat("https://example.com:bad", true); err == nil {
		t.Fatalf("expected invalid port to fail")
	}

	// 验证末尾斜杠被移除
	normalized, err := ValidateURLFormat("https://example.com/", false)
	if err != nil {
		t.Fatalf("expected trailing slash url to pass, got %v", err)
	}
	if normalized != "https://example.com" {
		t.Fatalf("expected trailing slash to be removed, got %s", normalized)
	}

	// 验证多个末尾斜杠被移除
	normalized, err = ValidateURLFormat("https://example.com///", false)
	if err != nil {
		t.Fatalf("expected multiple trailing slashes to pass, got %v", err)
	}
	if normalized != "https://example.com" {
		t.Fatalf("expected all trailing slashes to be removed, got %s", normalized)
	}

	// 验证带路径的 URL 末尾斜杠被移除
	normalized, err = ValidateURLFormat("https://example.com/api/v1/", false)
	if err != nil {
		t.Fatalf("expected trailing slash url with path to pass, got %v", err)
	}
	if normalized != "https://example.com/api/v1" {
		t.Fatalf("expected trailing slash to be removed from path, got %s", normalized)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	if _, err := ValidateHTTPURL("http://example.com", false, ValidationOptions{}); err == nil {
		t.Fatalf("expected http to fail when allow_insecure_http is false")
	}
	if _, err := ValidateHTTPURL("http://example.com", true, ValidationOptions{}); err != nil {
		t.Fatalf("expected http to pass when allow_insecure_http is true, got %v", err)
	}
	if _, err := ValidateHTTPURL("https://example.com", false, ValidationOptions{RequireAllowlist: true}); err == nil {
		t.Fatalf("expected require allowlist to fail when empty")
	}
	if _, err := ValidateHTTPURL("https://example.com", false, ValidationOptions{AllowedHosts: []string{"api.example.com"}}); err == nil {
		t.Fatalf("expected host not in allowlist to fail")
	}
	if _, err := ValidateHTTPURL("https://api.example.com", false, ValidationOptions{AllowedHosts: []string{"api.example.com"}}); err != nil {
		t.Fatalf("expected allowlisted host to pass, got %v", err)
	}
	if _, err := ValidateHTTPURL("https://sub.api.example.com", false, ValidationOptions{AllowedHosts: []string{"*.example.com"}}); err != nil {
		t.Fatalf("expected wildcard allowlist to pass, got %v", err)
	}
	if _, err := ValidateHTTPURL("https://localhost", false, ValidationOptions{AllowPrivate: false}); err == nil {
		t.Fatalf("expected localhost to be blocked when allow_private_hosts is false")
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"localhost", "LOCALHOST", "foo.localhost", "127.0.0.1", "::1", " 127.0.0.1 "} {
		if !IsLoopbackHost(host) {
			t.Fatalf("expected %q to be loopback", host)
		}
	}
	for _, host := range []string{"10.0.0.5", "192.168.1.1", "example.com", "8.8.8.8", ""} {
		if IsLoopbackHost(host) {
			t.Fatalf("expected %q not to be loopback", host)
		}
	}
}

func TestTrySameHostLoopbackURL(t *testing.T) {
	normalized, handled, err := TrySameHostLoopbackURL("http://127.0.0.1:18083")
	if err != nil {
		t.Fatalf("loopback http: %v", err)
	}
	if !handled || normalized != "http://127.0.0.1:18083" {
		t.Fatalf("got handled=%v normalized=%q", handled, normalized)
	}

	normalized, handled, err = TrySameHostLoopbackURL("http://[::1]:18083/")
	if err != nil {
		t.Fatalf("ipv6 loopback: %v", err)
	}
	if !handled || normalized != "http://[::1]:18083" {
		t.Fatalf("got handled=%v normalized=%q", handled, normalized)
	}

	if _, handled, err := TrySameHostLoopbackURL("https://api.openai.com"); handled || err != nil {
		t.Fatalf("public host should not be handled, handled=%v err=%v", handled, err)
	}
	if _, handled, err := TrySameHostLoopbackURL("http://192.168.1.10:18083"); handled || err != nil {
		t.Fatalf("private lan should not be handled, handled=%v err=%v", handled, err)
	}
}

func TestIsBlockedHost(t *testing.T) {
	for _, host := range []string{
		"localhost", "LOCALHOST", "foo.localhost", " 127.0.0.1 ",
		"127.0.0.1", "::1", "10.0.0.5", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "0.0.0.0", "::", "fe80::1", "fc00::1", "::ffff:127.0.0.1",
	} {
		if !IsBlockedHost(host) {
			t.Fatalf("expected %q to be blocked", host)
		}
	}
	for _, host := range []string{"example.com", "cdn.example.com", "93.184.216.34", "8.8.8.8", "2606:4700:4700::1111", ""} {
		if IsBlockedHost(host) {
			t.Fatalf("expected %q to be allowed", host)
		}
	}
}
