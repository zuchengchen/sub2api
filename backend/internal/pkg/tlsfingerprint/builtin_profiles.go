package tlsfingerprint

// BuiltinProfile returns a complete ClientHello template for the named
// compatibility preset.
//
// The codex-lane presets ("openssl36", "rustls", "rustls-h2") are calibrated
// against ClientHellos captured from the official Codex CLI 0.163.0 Linux
// build by backend/tools/codexgolden (see
// internal/pkg/codexwire/testdata/golden/0.163.0/tls_clienthellos*.json) and
// are pinned by golden tests in this package.
func BuiltinProfile(name string) *Profile {
	var p Profile
	switch name {
	case "nodejs24":
		p = Profile{
			Name:         "Node.js 24 compatibility",
			CipherSuites: []uint16{0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8, 0xc009, 0xc013, 0xc00a, 0xc014, 0x009c, 0x009d, 0x002f, 0x0035},
			Curves:       []uint16{29, 23, 24}, PointFormats: []uint16{0},
			SignatureAlgorithms: []uint16{0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601, 0x0201},
			ALPNProtocols:       []string{"http/1.1"}, SupportedVersions: []uint16{0x0304, 0x0303},
			KeyShareGroups: []uint16{29}, PSKModes: []uint16{1},
			Extensions: []uint16{0, 65037, 23, 65281, 10, 11, 35, 16, 5, 13, 18, 51, 45, 43},
		}
	case "nodejs22":
		p = Profile{
			Name:         "Node.js 22 compatibility",
			CipherSuites: []uint16{4866, 4867, 4865, 49199, 49195, 49200, 49196, 158, 49191, 103, 49192, 107, 163, 159, 52393, 52392, 52394, 49327, 49325, 49315, 49311, 49245, 49249, 49239, 49235, 162, 49326, 49324, 49314, 49310, 49244, 49248, 49238, 49234, 49188, 106, 49187, 64, 49162, 49172, 57, 56, 49161, 49171, 51, 50, 157, 49313, 49309, 49233, 156, 49312, 49308, 49232, 61, 60, 53, 47, 255},
			Curves:       []uint16{29, 23, 30, 25, 24, 256, 257, 258, 259, 260}, PointFormats: []uint16{0, 1, 2},
			SupportedVersions: []uint16{0x0304, 0x0303}, KeyShareGroups: []uint16{29}, PSKModes: []uint16{1},
			Extensions: []uint16{0, 11, 10, 35, 16, 22, 23, 13, 43, 45, 51},
		}
	case "openssl36", "openssl36-linux":
		// Official Codex CLI HTTP lane on Linux: reqwest default-tls →
		// native-tls → statically linked OpenSSL 3.6.x. native-tls-alpn is not
		// enabled, so no ALPN is offered and every request runs HTTP/1.1. The
		// extension order is fixed (OpenSSL does not shuffle), status_request is
		// not requested, and the X25519 key share is generated independently of
		// the X25519MLKEM768 hybrid share.
		p = Profile{
			Name: "OpenSSL 3.6 (Codex CLI HTTP, Linux)",
			CipherSuites: []uint16{
				0x1302, 0x1303, 0x1301,
				0xc02c, 0xc030, 0x009f, 0xcca9, 0xcca8, 0xccaa,
				0xc02b, 0xc02f, 0x009e,
				0xc024, 0xc028, 0x006b, 0xc023, 0xc027, 0x0067,
				0xc00a, 0xc014, 0x0039, 0xc009, 0xc013, 0x0033,
				0x009d, 0x009c, 0x003d, 0x003c, 0x0035, 0x002f,
			},
			Curves:       []uint16{0x11ec, 0x001d, 0x0017, 0x001e, 0x0018, 0x0019, 0x0100, 0x0101},
			PointFormats: []uint16{0},
			SignatureAlgorithms: []uint16{
				0x0905, 0x0906, 0x0904,
				0x0403, 0x0503, 0x0603,
				0x0807, 0x0808,
				0x081a, 0x081b, 0x081c,
				0x0809, 0x080a, 0x080b,
				0x0804, 0x0805, 0x0806,
				0x0401, 0x0501, 0x0601,
				0x0303, 0x0301, 0x0302, 0x0402, 0x0502, 0x0602,
			},
			SupportedVersions: []uint16{0x0304, 0x0303},
			KeyShareGroups:    []uint16{0x11ec, 0x001d},
			PSKModes:          []uint16{1},
			// renegotiation_info, server_name, ec_point_formats,
			// supported_groups, session_ticket, encrypt_then_mac,
			// extended_master_secret, signature_algorithms, supported_versions,
			// psk_key_exchange_modes, key_share.
			Extensions: []uint16{0xff01, 0, 11, 10, 35, 22, 23, 13, 43, 45, 51},
			H1:         true,
		}
	case "rustls", "rustls-h2":
		// Official Codex CLI rustls 0.23.x + aws-lc-rs ClientHello. The WS lane
		// always uses it (websocket-client builds a rustls config); the HTTP lane
		// only switches to rustls when a custom CA bundle is configured
		// (CODEX_CA_CERTIFICATE / SSL_CERT_FILE), in which case reqwest offers
		// ALPN [h2, http/1.1] — that is the "rustls-h2" variant.
		//   - cipher suites end with TLS_EMPTY_RENEGOTIATION_INFO_SCSV (0x00ff);
		//     rustls never sends renegotiation_info.
		//   - signature algorithms follow aws-lc-rs' verifier order and include
		//     ML-DSA-44/65/87 (0x0904-0x0906).
		//   - the X25519 key share is the classical half of the
		//     X25519MLKEM768 share (rustls hybrid key reuse).
		//   - extensions are shuffled per connection (order_seed).
		exts := []uint16{0, 11, 10, 35, 5, 13, 23, 51, 45, 43}
		alpn := []string{}
		if name == "rustls-h2" {
			exts = append(exts, 16)
			alpn = []string{"h2", "http/1.1"}
		}
		p = Profile{
			Name: "rustls 0.23 aws-lc (Codex CLI)",
			CipherSuites: []uint16{
				0x1302, 0x1301, 0x1303,
				0xc02c, 0xc02b, 0xcca9,
				0xc030, 0xc02f, 0xcca8,
				0x00ff,
			},
			Curves:       []uint16{0x11ec, 29, 23, 24},
			PointFormats: []uint16{0},
			SignatureAlgorithms: []uint16{
				0x0503, 0x0403, 0x0603,
				0x0807,
				0x0806, 0x0805, 0x0804,
				0x0601, 0x0501, 0x0401,
				0x0904, 0x0905, 0x0906,
			},
			ALPNProtocols:     alpn,
			SupportedVersions: []uint16{0x0304, 0x0303},
			KeyShareGroups:    []uint16{0x11ec, 29},
			PSKModes:          []uint16{1},
			Extensions:        exts,
			ShuffleExtensions: true,
			ReuseHybridX25519: true,
		}
		if name == "rustls-h2" {
			p.Name = "rustls 0.23 aws-lc h2 (Codex CLI HTTP, custom CA)"
			p.H2 = true
		}
	default:
		return nil
	}
	return &p
}
