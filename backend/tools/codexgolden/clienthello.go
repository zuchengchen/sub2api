//go:build linux

package main

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// clientHello is the parsed shape of one captured TLS ClientHello. Numeric
// fields are kept as IANA code points so they can be fed back into the
// sub2api tlsfingerprint profiles verbatim.
type clientHello struct {
	Lane              string          `json:"lane"`
	RecordVersion     uint16          `json:"record_version"`
	LegacyVersion     uint16          `json:"legacy_version"`
	SessionIDLen      int             `json:"session_id_len"`
	CipherSuites      []uint16        `json:"cipher_suites"`
	Compression       []uint8         `json:"compression_methods"`
	Extensions        []uint16        `json:"extensions"`
	ServerName        string          `json:"server_name,omitempty"`
	ALPN              []string        `json:"alpn,omitempty"`
	SupportedGroups   []uint16        `json:"supported_groups,omitempty"`
	SignatureSchemes  []uint16        `json:"signature_algorithms,omitempty"`
	SupportedVersions []uint16        `json:"supported_versions,omitempty"`
	PSKModes          []uint8         `json:"psk_key_exchange_modes,omitempty"`
	PointFormats      []uint8         `json:"ec_point_formats,omitempty"`
	KeyShares         []keyShareEntry `json:"key_shares,omitempty"`
	// HybridReusesX25519 reports whether the standalone X25519 key share equals
	// the classical (trailing) half of the X25519MLKEM768 share.
	HybridReusesX25519 *bool  `json:"hybrid_reuses_x25519,omitempty"`
	StatusRequest      bool   `json:"status_request"`
	Raw                string `json:"raw_hex"`
}

type keyShareEntry struct {
	Group  uint16 `json:"group"`
	KeyLen int    `json:"key_len"`
}

const (
	groupX25519         = 0x001d
	groupX25519MLKEM768 = 0x11ec
	mlkem768EncapKeyLen = 1184
)

// parseClientHello parses a single TLS record holding a ClientHello.
func parseClientHello(record []byte) (*clientHello, error) {
	if len(record) < 5 || record[0] != 0x16 {
		return nil, errors.New("not a TLS handshake record")
	}
	ch := &clientHello{RecordVersion: binary.BigEndian.Uint16(record[1:3]), Raw: hex.EncodeToString(record)}
	recLen := int(binary.BigEndian.Uint16(record[3:5]))
	if len(record) < 5+recLen {
		return nil, fmt.Errorf("short record: have %d want %d", len(record), 5+recLen)
	}
	hs := record[5 : 5+recLen]
	if len(hs) < 4 || hs[0] != 0x01 {
		return nil, errors.New("not a ClientHello")
	}
	bodyLen := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
	if len(hs) < 4+bodyLen {
		return nil, errors.New("ClientHello spans multiple records (unsupported)")
	}
	r := reader{b: hs[4 : 4+bodyLen]}
	ch.LegacyVersion = r.u16()
	r.skip(32) // random
	ch.SessionIDLen = int(r.u8())
	r.skip(ch.SessionIDLen)
	suites := r.vec16()
	for len(suites) >= 2 {
		ch.CipherSuites = append(ch.CipherSuites, binary.BigEndian.Uint16(suites))
		suites = suites[2:]
	}
	ch.Compression = append([]uint8(nil), r.vec8()...)
	exts := reader{b: r.vec16()}
	if r.err != nil {
		return nil, r.err
	}
	var hybridX25519, plainX25519 []byte
	for len(exts.b) > 0 && exts.err == nil {
		typ := exts.u16()
		data := reader{b: exts.vec16()}
		ch.Extensions = append(ch.Extensions, typ)
		switch typ {
		case 0x0000: // server_name
			list := reader{b: data.vec16()}
			for len(list.b) > 0 && list.err == nil {
				nameType := list.u8()
				name := list.vec16()
				if nameType == 0 {
					ch.ServerName = string(name)
				}
			}
		case 0x0005:
			ch.StatusRequest = true
		case 0x000a:
			list := data.vec16()
			for len(list) >= 2 {
				ch.SupportedGroups = append(ch.SupportedGroups, binary.BigEndian.Uint16(list))
				list = list[2:]
			}
		case 0x000b:
			ch.PointFormats = append([]uint8(nil), data.vec8()...)
		case 0x000d:
			list := data.vec16()
			for len(list) >= 2 {
				ch.SignatureSchemes = append(ch.SignatureSchemes, binary.BigEndian.Uint16(list))
				list = list[2:]
			}
		case 0x0010:
			list := reader{b: data.vec16()}
			for len(list.b) > 0 && list.err == nil {
				ch.ALPN = append(ch.ALPN, string(list.vec8()))
			}
		case 0x002b:
			list := data.vec8()
			for len(list) >= 2 {
				ch.SupportedVersions = append(ch.SupportedVersions, binary.BigEndian.Uint16(list))
				list = list[2:]
			}
		case 0x002d:
			ch.PSKModes = append([]uint8(nil), data.vec8()...)
		case 0x0033:
			list := reader{b: data.vec16()}
			for len(list.b) > 0 && list.err == nil {
				group := list.u16()
				key := list.vec16()
				ch.KeyShares = append(ch.KeyShares, keyShareEntry{Group: group, KeyLen: len(key)})
				switch group {
				case groupX25519MLKEM768:
					if len(key) == mlkem768EncapKeyLen+32 {
						hybridX25519 = key[mlkem768EncapKeyLen:]
					}
				case groupX25519:
					plainX25519 = key
				}
			}
		}
	}
	if exts.err != nil {
		return nil, exts.err
	}
	if hybridX25519 != nil && plainX25519 != nil {
		reused := string(hybridX25519) == string(plainX25519)
		ch.HybridReusesX25519 = &reused
	}
	ch.Lane = classifyLane(ch)
	return ch, nil
}

// classifyLane names the TLS library family. rustls never sends
// renegotiation_info (65281) and signals renegotiation via the 0x00ff SCSV;
// OpenSSL sends the extension instead.
func classifyLane(ch *clientHello) string {
	for _, e := range ch.Extensions {
		if e == 0xff01 {
			return "openssl"
		}
	}
	for _, s := range ch.CipherSuites {
		if s == 0x00ff {
			return "rustls"
		}
	}
	return "unknown"
}

// fingerprintKey dedupes hellos that differ only in randomness. Extension
// order is excluded only for rustls, which shuffles it per connection. Every
// other parsed field participates, so aggregation cannot hide a bad sample.
func (ch *clientHello) fingerprintKey() string {
	shape := *ch
	shape.Raw = ""
	if ch.Lane == "rustls" {
		shape.Extensions = append([]uint16(nil), ch.Extensions...)
		sort.Slice(shape.Extensions, func(i, j int) bool { return shape.Extensions[i] < shape.Extensions[j] })
	}
	encoded, err := json.Marshal(shape)
	if err != nil {
		panic(err)
	} // fixed struct has no fallible JSON values
	return string(encoded)
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) need(n int) bool {
	if r.err != nil {
		return false
	}
	if len(r.b) < n {
		r.err = errors.New("truncated ClientHello")
		return false
	}
	return true
}

func (r *reader) u8() uint8 {
	if !r.need(1) {
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func (r *reader) u16() uint16 {
	if !r.need(2) {
		return 0
	}
	v := binary.BigEndian.Uint16(r.b)
	r.b = r.b[2:]
	return v
}

func (r *reader) skip(n int) {
	if r.need(n) {
		r.b = r.b[n:]
	}
}

func (r *reader) vec8() []byte {
	n := int(r.u8())
	if !r.need(n) {
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) vec16() []byte {
	n := int(r.u16())
	if !r.need(n) {
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func hexList(vals []uint16) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = fmt.Sprintf("%04x", v)
	}
	return strings.Join(parts, ",")
}
