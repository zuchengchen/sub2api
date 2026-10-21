package tlsfingerprint

// BuiltinProfile returns a complete ClientHello template for the named
// compatibility preset. The names describe template families, not verified
// captures of an official client.
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
	case "rustls", "rustls-h2":
		// rustls 0.23.x + aws-lc-rs 默认 ClientHello（Codex CLI：WS 走
		// websocket-client 的 build_rustls_client_config_with_custom_ca，HTTP 走
		// reqwest 的 rustls connector，同一 TLS 栈）：
		//   cipher order: TLS13 AES256/AES128/CHACHA, then ECDHE_ECDSA AES256/AES128/CHACHA,
		//   then ECDHE_RSA AES256/AES128/CHACHA（rustls DEFAULT_CIPHER_SUITES 序）。
		//   supported_groups 首项 X25519MLKEM768（aws-lc-rs 默认开后量子 kx），
		//   key_share 只投前两个组。
		//   WS 场景不发 ALPN（alpn_protocols 为空 → 无 16 扩展）；HTTP 场景
		//   reqwest 发 ALPN [h2, http/1.1]。
		//   extended_master_secret(23)、status_request/OCSP(5) 与 session_ticket(35)
		//   由 rustls 默认开启（hs.rs emit_client_hello_for_retry 无条件发 EMS 与
		//   CertificateStatusRequest::build_ocsp；session_storage 默认开）。
		//   扩展集合在每次握手按 rustls 0.23 的 order_seed 机制随机排序——
		//   Extensions 列表只是无序集合，真实线序由 ShuffleExtensions 产生。
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
			},
			Curves:       []uint16{0x11ec, 29, 23, 24},
			PointFormats: []uint16{0},
			SignatureAlgorithms: []uint16{
				0x0403, 0x0807, 0x0503, 0x0603,
				0x0804, 0x0805, 0x0806,
				0x0401, 0x0501, 0x0601,
			},
			ALPNProtocols:     alpn,
			SupportedVersions: []uint16{0x0304, 0x0303},
			KeyShareGroups:    []uint16{0x11ec, 29},
			PSKModes:          []uint16{1},
			Extensions:        exts,
			ShuffleExtensions: true,
		}
		if name == "rustls-h2" {
			p.Name = "rustls 0.23 aws-lc h2 (Codex CLI HTTP)"
			p.H2 = true
		}
	default:
		return nil
	}
	return &p
}
