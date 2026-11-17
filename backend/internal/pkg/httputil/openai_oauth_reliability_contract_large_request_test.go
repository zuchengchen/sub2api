package httputil

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

// Build large, public synthetic fixtures without also retaining an uncompressed
// 64 MiB request. The digest verifies all decoded bytes, including the JSON tail.
func oauthReliabilityEncodedBody(t *testing.T, encoding string, size int, completePrefix bool) ([]byte, [sha256.Size]byte) {
	t.Helper()
	var wire bytes.Buffer
	var writer io.WriteCloser
	var err error
	switch encoding {
	case "gzip":
		writer, err = gzip.NewWriterLevel(&wire, gzip.BestSpeed)
	case "deflate":
		writer, err = zlib.NewWriterLevel(&wire, zlib.BestSpeed)
	case "zstd":
		writer, err = zstd.NewWriter(&wire, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
	default:
		t.Fatalf("unsupported fixture encoding %q", encoding)
	}
	require.NoError(t, err)
	digest := sha256.New()
	out := io.MultiWriter(writer, digest)
	prefix, suffix, fill := `{"model":"gpt-5.1","input":"synthetic boundary `, `"}`, byte('x')
	if completePrefix {
		prefix, suffix, fill = `{"model":"gpt-5.1","input":"complete JSON before padding"}`, "", ' '
	}
	require.GreaterOrEqual(t, size, len(prefix)+len(suffix))
	_, err = io.WriteString(out, prefix)
	require.NoError(t, err)
	chunk := bytes.Repeat([]byte{fill}, 64<<10)
	for remaining := size - len(prefix) - len(suffix); remaining > 0; {
		n := min(remaining, len(chunk))
		_, err = out.Write(chunk[:n])
		require.NoError(t, err)
		remaining -= n
	}
	_, err = io.WriteString(out, suffix)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	var sum [sha256.Size]byte
	copy(sum[:], digest.Sum(nil))
	return wire.Bytes(), sum
}

func oauthReliabilityDecodeBody(t *testing.T, encoding string, wire []byte, limit int64) ([]byte, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(wire))
	req.Header.Set("Content-Encoding", encoding)
	req.Header.Set("Content-Type", "application/json")
	body, err := ReadLenientJSONRequestBodyWithPrealloc(req, limit)
	if err == nil {
		require.Empty(t, req.Header.Get("Content-Encoding"))
		require.EqualValues(t, len(body), req.ContentLength)
	}
	return body, err
}

func TestOAuthReliabilityLargeRequestDecodedLimit(t *testing.T) {
	for _, encoding := range []string{"gzip", "zstd", "deflate"} {
		for _, tc := range []struct {
			name           string
			size           int
			completePrefix bool
		}{
			{"below", maxDecompressedBodySize - 1, false},
			{"exact", maxDecompressedBodySize, false},
			{"over", maxDecompressedBodySize + 1, false},
			{"over_complete_json_prefix", maxDecompressedBodySize + 1, true},
		} {
			t.Run(encoding+"/"+tc.name, func(t *testing.T) {
				wire, digest := oauthReliabilityEncodedBody(t, encoding, tc.size, tc.completePrefix)
				body, err := oauthReliabilityDecodeBody(t, encoding, wire, 256<<20)
				if tc.size > maxDecompressedBodySize {
					var limitErr *http.MaxBytesError
					require.ErrorAs(t, err, &limitErr, "oversize must not return a truncated JSON prefix or an apparent success")
					require.EqualValues(t, maxDecompressedBodySize, limitErr.Limit)
					require.Nil(t, body)
					return
				}
				require.NoError(t, err)
				require.Len(t, body, tc.size)
				require.Equal(t, digest, sha256.Sum256(body), "the entire permitted request must survive decoding")
			})
		}
	}
}

func TestOAuthReliabilityLargeRequestConfiguredLimit(t *testing.T) {
	const limit = 4096
	for _, encoding := range []string{"gzip", "zstd", "deflate"} {
		for _, size := range []int{limit - 1, limit, limit + 1} {
			t.Run(fmt.Sprintf("%s/%d", encoding, size), func(t *testing.T) {
				wire, digest := oauthReliabilityEncodedBody(t, encoding, size, false)
				body, err := oauthReliabilityDecodeBody(t, encoding, wire, limit)
				if size > limit {
					var limitErr *http.MaxBytesError
					require.ErrorAs(t, err, &limitErr)
					require.EqualValues(t, limit, limitErr.Limit)
					require.Nil(t, body)
					return
				}
				require.NoError(t, err)
				require.Equal(t, digest, sha256.Sum256(body))
			})
		}
	}
}

func TestOAuthReliabilityLargeRequestIdentityKeepsConfiguredBudget(t *testing.T) {
	// The 64 MiB ceiling protects decompression. It must not silently become
	// an upstream or plain-JSON request limit when the gateway allows more.
	plain := append([]byte(`{"model":"gpt-5.1","input":"`), bytes.Repeat([]byte{'x'}, maxDecompressedBodySize)...)
	plain = append(plain, '"', '}')
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(plain))
	body, err := ReadLenientJSONRequestBodyWithPrealloc(req, 256<<20)
	require.NoError(t, err)
	require.Len(t, body, len(plain))
	require.Equal(t, sha256.Sum256(plain), sha256.Sum256(body))
}

func TestOAuthReliabilityLargeRequestDecoderTailFailure(t *testing.T) {
	for _, encoding := range []string{"gzip", "zstd", "deflate"} {
		for _, size := range []int{4096, maxDecompressedBodySize} {
			t.Run(fmt.Sprintf("%s/%d", encoding, size), func(t *testing.T) {
				wire, _ := oauthReliabilityEncodedBody(t, encoding, size, false)
				for _, failure := range []string{"checksum", "truncated_trailer"} {
					t.Run(failure, func(t *testing.T) {
						broken := bytes.Clone(wire)
						if failure == "checksum" {
							broken[len(broken)-4] ^= 0x80
						} else {
							broken = broken[:len(broken)-1]
						}
						body, err := oauthReliabilityDecodeBody(t, encoding, broken, 256<<20)
						require.Error(t, err, "reading exactly the limit must still validate the compressed stream tail")
						var limitErr *http.MaxBytesError
						require.NotErrorAs(t, err, &limitErr, "a corrupt permitted-size body is a decode failure, not a size overflow")
						require.Nil(t, body)
					})
				}
			})
		}
	}
}
