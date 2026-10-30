package codexwire

import (
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestCodexCustomCAMixedBundleRejectsMalformedSections(t *testing.T) {
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()
	valid := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	path := filepath.Join(t.TempDir(), "bundle.pem")
	t.Setenv("CODEX_CA_CERTIFICATE", path)
	t.Setenv("SSL_CERT_FILE", "")
	for _, label := range []string{"CERTIFICATE", "X509 CRL", "PRIVATE KEY"} {
		for _, first := range []bool{false, true} {
			bad := []byte("-----BEGIN " + label + "-----\n!not-base64!\n-----END " + label + "-----\n")
			bundle := append(append([]byte{}, valid...), bad...)
			if first {
				bundle = append(append([]byte{}, bad...), valid...)
			}
			require.NoError(t, os.WriteFile(path, bundle, 0600))
			_, err := WithCodexCustomCA(tlsfingerprint.BuiltinProfile("rustls-h2"))
			require.ErrorContains(t, err, "PEM section")
		}
	}
	// Well-formed CRL PEM sections are classified and ignored, as in Codex;
	// comments and a trusted certificate's X509_AUX suffix are accepted.
	trusted := pem.EncodeToMemory(&pem.Block{Type: "TRUSTED CERTIFICATE", Bytes: append(append([]byte{}, srv.Certificate().Raw...), 0x30, 0x00)})
	bundle := append([]byte("# local CA bundle\n"), trusted...)
	bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: []byte{0x30, 0x00}})...)
	require.NoError(t, os.WriteFile(path, bundle, 0600))
	profile, err := WithCodexCustomCA(tlsfingerprint.BuiltinProfile("rustls-h2"))
	require.NoError(t, err)
	require.NotNil(t, profile.RootCAs)
	require.False(t, profile.InsecureSkipVerify)
	unknown := "-----BEGIN UNRELATED DATA-----\r!not-base64!\r-----END UNRELATED DATA-----\r"
	certs, err := codexPEMCertificates(append([]byte(unknown), valid...))
	require.NoError(t, err, "unknown sections are ignored without decoding")
	require.Len(t, certs, 1)
	_, err = codexPEMCertificates(append(append([]byte{}, valid...), []byte("-----BEGIN UNRELATED DATA-----\nmissing end")...))
	require.ErrorContains(t, err, "unterminated")
	_, err = codexPEMCertificates(append(append([]byte{}, valid...), []byte("-----BEGIN UNRELATED DATA----\n")...))
	require.ErrorContains(t, err, "section start")
}
