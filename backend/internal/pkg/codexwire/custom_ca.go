package codexwire

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"os"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// CodexCustomCAConfigured follows the CLI's precedence, including empty-value
// handling. It does not change process-wide trust or other clients.
func CodexCustomCAConfigured() bool {
	return os.Getenv("CODEX_CA_CERTIFICATE") != "" || os.Getenv("SSL_CERT_FILE") != ""
}

var codexRootCache struct {
	sync.Mutex
	key   string
	roots *x509.CertPool
}

// WithCodexCustomCA adds the selected CA bundle to a copy of the profile.
// Trust is cached by bundle contents, so replacing a file reloads trust and
// changes the connection-pool identity. Invalid configured bundles fail closed.
func WithCodexCustomCA(profile *tlsfingerprint.Profile) (*tlsfingerprint.Profile, error) {
	if profile == nil || !CodexCustomCAConfigured() {
		return profile, nil
	}
	key, path := "CODEX_CA_CERTIFICATE", os.Getenv("CODEX_CA_CERTIFICATE")
	if path == "" {
		key, path = "SSL_CERT_FILE", os.Getenv("SSL_CERT_FILE")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load Codex CA bundle from %s: %w", key, err)
	}
	id := fmt.Sprintf("%x", sha256.Sum256(data))
	codexRootCache.Lock()
	defer codexRootCache.Unlock()
	if codexRootCache.key != id || codexRootCache.roots == nil {
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load Codex system roots: %w", err)
		}
		certs, err := codexPEMCertificates(data)
		if err != nil {
			return nil, err
		}
		certificates := len(certs)
		for _, cert := range certs {
			roots.AddCert(cert)
		}
		if certificates == 0 {
			return nil, fmt.Errorf("Codex CA bundle from %s contains no certificates", key)
		}
		codexRootCache.key, codexRootCache.roots = id, roots
	}
	next := *profile
	next.RootCAs, next.RootCAID = codexRootCache.roots, id
	return &next, nil
}

// Parse each section in order: malformed recognized sections are errors, while
// unknown labels are skipped without base64 decoding. This mirrors the mixed
// section iterator used by Codex, rather than pem.Decode's skip-to-valid search.
func codexPEMCertificates(data []byte) ([]*x509.Certificate, error) {
	trusted := bytes.Contains(data, []byte("TRUSTED CERTIFICATE"))
	if trusted {
		data = bytes.ReplaceAll(data, []byte("BEGIN TRUSTED CERTIFICATE"), []byte("BEGIN CERTIFICATE"))
		data = bytes.ReplaceAll(data, []byte("END TRUSTED CERTIFICATE"), []byte("END CERTIFICATE"))
	}
	var certificates []*x509.Certificate
	var encoded []byte
	label, active := "", false
	for len(data) > 0 {
		line := data
		if end := bytes.IndexAny(data, "\r\n"); end >= 0 {
			line, data = data[:end], data[end+1:]
		} else {
			data = nil
		}
		if bytes.HasPrefix(line, []byte("-----BEGIN ")) {
			dashes, pos := 0, len(line)
			for i := len(line) - 1; i >= 0; i-- {
				if line[i] == '-' {
					dashes++
					pos = i
				} else if line[i] != ' ' {
					break
				}
			}
			if dashes != 5 || pos < 11 {
				return nil, fmt.Errorf("invalid Codex CA PEM section start")
			}
			label, active = string(line[11:pos]), true
			continue
		}
		if !active {
			continue
		}
		if !bytes.HasPrefix(line, []byte("-----END "+label+"-----")) {
			encoded = append(encoded, line...)
			encoded = append(encoded, '\n')
			continue
		}
		active = false
		switch label {
		case "CERTIFICATE", "PUBLIC KEY", "RSA PRIVATE KEY", "PRIVATE KEY", "EC PRIVATE KEY", "X509 CRL", "CERTIFICATE REQUEST", "ECHCONFIG":
		default:
			encoded = encoded[:0]
			continue
		}
		section := append([]byte("-----BEGIN "+label+"-----\n"), encoded...)
		section = append(section, []byte("-----END "+label+"-----\n")...)
		encoded = encoded[:0]
		block, _ := pem.Decode(section)
		if block == nil {
			return nil, fmt.Errorf("invalid Codex CA PEM section %s", label)
		}
		if label != "CERTIFICATE" {
			continue
		}
		der := block.Bytes
		if trusted {
			var first asn1.RawValue
			if _, err := asn1.Unmarshal(der, &first); err != nil {
				return nil, fmt.Errorf("invalid trusted Codex CA certificate: %w", err)
			}
			der = first.FullBytes
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("invalid Codex CA certificate: %w", err)
		}
		certificates = append(certificates, cert)
	}
	if active {
		return nil, fmt.Errorf("unterminated Codex CA PEM section")
	}
	return certificates, nil
}
