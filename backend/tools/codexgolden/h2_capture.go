//go:build linux

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func customCARecording() bool { return os.Getenv("CODEXGOLDEN_CUSTOM_CA") == "1" }
func recorderTLSMode() string {
	if customCARecording() {
		return "custom_ca_rustls_h2"
	}
	return "system_roots_namespace"
}
func recorderALPN() []string {
	if customCARecording() {
		return []string{"h2", "http/1.1"}
	}
	return []string{"http/1.1"}
}

type h2Setting struct {
	ID    uint16 `json:"id"`
	Value uint32 `json:"value"`
}
type h2Observation struct {
	StreamID                  uint32      `json:"stream_id"`
	Settings                  []h2Setting `json:"settings"`
	ConnectionWindowIncrement uint32      `json:"connection_window_increment"`
	Headers                   []headerKV  `json:"-"`
	RequestIndex              int         `json:"-"`
}

// This marker exists only between the wire observer and the in-process HTTP/2
// server. It is never recorded or sent to a client or gateway. net/http does not
// expose the request stream ID; matching handlers by method/path/UA confuses
// concurrent requests whose bodies or other headers differ.
const h2ObservationHeader = "x-codexgolden-observed-stream"

type h2Observations struct {
	mu      sync.Mutex
	pending map[uint32]*h2Observation
}

func (o *h2Observations) add(observation *h2Observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pending[observation.StreamID] = observation
}

func (o *h2Observations) take(r *http.Request) (*h2Observation, error) {
	values := r.Header.Values(h2ObservationHeader)
	r.Header.Del(h2ObservationHeader)
	if len(values) != 1 {
		return nil, errors.New("missing real HTTP/2 stream association")
	}
	id, err := strconv.ParseUint(values[0], 10, 32)
	if err != nil {
		return nil, errors.New("invalid HTTP/2 stream association")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	observation := o.pending[uint32(id)]
	delete(o.pending, uint32(id))
	if observation == nil {
		return nil, errors.New("missing or reused HTTP/2 observation")
	}
	return observation, nil
}

// h2ReadEnd distinguishes idle connection shutdown from lost application bytes.
// A peer reset is acceptable only before any preface byte, or exactly between
// complete frames with no unfinished request stream. Partial prefaces, header
// continuations, DATA payloads and unfinished streams remain failed evidence.
type h2ReadEnd struct {
	FrameTraceVersion  int              `json:"frame_trace_version"`
	FrameDirection     string           `json:"frame_direction"`
	FrameTraceLimit    int              `json:"frame_trace_limit"`
	FrameTraceOverflow bool             `json:"frame_trace_overflow"`
	Frames             []h2FrameSummary `json:"frames"`

	Cause           error    `json:"-"`
	Phase           string   `json:"phase"`
	PartialBytes    int      `json:"partial_bytes"`
	CompletedFrames int      `json:"completed_frames"`
	PendingStreams  []uint32 `json:"pending_streams"`
	CleanPeerClose  bool     `json:"clean_peer_close"`
}

func (e *h2ReadEnd) Error() string {
	return fmt.Sprintf("%s read stopped after %d partial bytes, %d complete frames, %d unfinished streams: %v", e.Phase, e.PartialBytes, e.CompletedFrames, len(e.PendingStreams), e.Cause)
}
func (e *h2ReadEnd) Unwrap() error { return e.Cause }

func observedH2ReadEnd(cause error, phase string, partial, completed int, pending map[uint32]bool, trace *h2FrameTrace) error {
	ids := make([]uint32, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	clean := partial == 0 && len(ids) == 0 && (errors.Is(cause, io.EOF) || errors.Is(cause, syscall.ECONNRESET))
	return &h2ReadEnd{Cause: cause, Phase: phase, PartialBytes: partial, CompletedFrames: completed, PendingStreams: ids, CleanPeerClose: clean && !trace.overflow, FrameTraceVersion: h2FrameTraceVersion, FrameDirection: "client_to_fixture", FrameTraceLimit: trace.limit, FrameTraceOverflow: trace.overflow, Frames: trace.snapshot()}
}

func completeH2FrameCount(raw []byte) int {
	count := 0
	for len(raw) >= 9 {
		n := int(raw[0])<<16 | int(raw[1])<<8 | int(raw[2])
		if len(raw) < 9+n {
			break
		}
		count++
		raw = raw[9+n:]
	}
	return count
}

// relayObservedH2 decodes the actual decrypted frames first, retaining their
// header order, SETTINGS and stream IDs. Only the internal parser's copy of
// initial HEADERS gains a stream marker; all recorded headers remain original.
// A separate HPACK encoder preserves internal dynamic-table consistency when
// header blocks span CONTINUATION frames or reuse the client's dynamic table.
func relayObservedH2(src io.Reader, dst io.Writer, observations *h2Observations) error {
	trace := &h2FrameTrace{limit: maxH2FrameTraceFrames}
	preface := make([]byte, len(http2.ClientPreface))
	if n, err := io.ReadFull(src, preface); err != nil {
		return observedH2ReadEnd(err, "preface", n, 0, nil, trace)
	}
	if string(preface) != http2.ClientPreface {
		return errors.New("invalid HTTP/2 preface")
	}
	if _, err := dst.Write(preface); err != nil {
		return err
	}
	var raw, encoded bytes.Buffer
	fr := http2.NewFramer(io.Discard, io.TeeReader(src, &raw))
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	fw := http2.NewFramer(dst, nil)
	encoder := hpack.NewEncoder(&encoded)
	var settings []h2Setting
	var window uint32
	seen := map[uint32]bool{}
	pending := map[uint32]bool{}
	completedFrames := 0
	index := 0
	for {
		raw.Reset()
		frame, err := fr.ReadFrame()
		completedFrames += completeH2FrameCount(raw.Bytes())
		_, traceErr := trace.observeRead(raw.Bytes())
		if traceErr != nil {
			return observedH2ReadEnd(traceErr, "frame_trace", raw.Len(), completedFrames, pending, trace)
		}
		if err != nil {
			return observedH2ReadEnd(err, "frame", raw.Len(), completedFrames, pending, trace)
		}
		switch v := frame.(type) {
		case *http2.DataFrame:
			if v.StreamEnded() {
				delete(pending, v.StreamID)
			}
		case *http2.RSTStreamFrame:
			delete(pending, v.StreamID)
		case *http2.SettingsFrame:
			if !v.IsAck() {
				if err := v.ForeachSetting(func(s http2.Setting) error {
					settings = append(settings, h2Setting{uint16(s.ID), s.Val})
					return nil
				}); err != nil {
					return err
				}
			}
		case *http2.WindowUpdateFrame:
			if v.StreamID == 0 && index == 0 {
				window += v.Increment
			}
		case *http2.MetaHeadersFrame:
			if v.Truncated {
				return errors.New("truncated HTTP/2 header observation")
			}
			encoded.Reset()
			if v.StreamEnded() {
				delete(pending, v.StreamID)
			} else {
				pending[v.StreamID] = true
			}
			initial := !seen[v.StreamID]
			var observation *h2Observation
			if initial {
				index++
				observation = &h2Observation{StreamID: v.StreamID, Settings: append([]h2Setting(nil), settings...), ConnectionWindowIncrement: window, RequestIndex: index}
				seen[v.StreamID] = true
			}
			for _, h := range v.Fields {
				if h.Name == h2ObservationHeader {
					return errors.New("wire header collides with internal HTTP/2 observation marker")
				}
				if observation != nil {
					observation.Headers = append(observation.Headers, headerKV{h.Name, h.Value})
				}
				if err := encoder.WriteField(h); err != nil {
					return err
				}
			}
			if observation != nil {
				observations.add(observation)
				if err := encoder.WriteField(hpack.HeaderField{Name: h2ObservationHeader, Value: strconv.FormatUint(uint64(v.StreamID), 10)}); err != nil {
					return err
				}
			}
			block := encoded.Bytes()
			const maxFramePayload = 16384
			n := min(len(block), maxFramePayload)
			if err := fw.WriteHeaders(http2.HeadersFrameParam{StreamID: v.StreamID, BlockFragment: block[:n], EndStream: v.StreamEnded(), EndHeaders: n == len(block)}); err != nil {
				return err
			}
			for block = block[n:]; len(block) > 0; block = block[n:] {
				n = min(len(block), maxFramePayload)
				if err := fw.WriteContinuation(v.StreamID, n == len(block), block[:n]); err != nil {
					return err
				}
			}
			continue
		}
		if _, err := dst.Write(raw.Bytes()); err != nil {
			return err
		}
	}
}

// The internal HTTP/1 pipe only reuses fixture responses. Its bytes and generated
// framing are never used as HTTP/2 evidence.
func (f *fakeUpstream) serveH2(conn net.Conn, id int, front bool) {
	pr, pw := io.Pipe()
	observations := &h2Observations{pending: map[uint32]*h2Observation{}}
	parsed := make(chan error, 1)
	go func() {
		err := relayObservedH2(conn, pw, observations)
		_ = pw.CloseWithError(err)
		parsed <- err
	}()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o, err := observations.take(r)
		if err != nil {
			f.captureError(err)
			http.Error(w, "missing real HTTP/2 observation", 500)
			return
		}
		delivered := false
		defer func() { f.confirmNativeH2(id, o.StreamID, delivered) }()
		body, err := io.ReadAll(io.LimitReader(r.Body, (32<<20)+1))
		if err != nil || len(body) > 32<<20 {
			f.captureError(errors.New("HTTP/2 fixture body unreadable or exceeds 32 MiB"))
			http.Error(w, "read fixture request", 500)
			return
		}
		client, server := net.Pipe()
		defer client.Close()
		go func() { defer server.Close(); f.handleObservedConn(server, id, front, o) }()
		go func() {
			var head bytes.Buffer
			fmt.Fprintf(&head, "%s %s HTTP/1.1\r\n", r.Method, r.RequestURI)
			for _, h := range o.Headers {
				if strings.HasPrefix(h.Name, ":") || h.Name == "content-length" {
					continue
				}
				fmt.Fprintf(&head, "%s: %s\r\n", h.Name, h.Value)
			}
			fmt.Fprintf(&head, "content-length: %d\r\n\r\n", len(body))
			head.Write(body)
			_, _ = client.Write(head.Bytes())
		}()
		resp, err := http.ReadResponse(bufio.NewReader(client), r)
		if err != nil {
			f.captureError(fmt.Errorf("HTTP/2 fixture response: %w", err))
			http.Error(w, "scripted fixture response", 500)
			return
		}
		defer resp.Body.Close()
		for name, values := range resp.Header {
			if name == "Connection" || name == "Transfer-Encoding" {
				continue
			}
			w.Header()[name] = values
		}
		w.WriteHeader(resp.StatusCode)
		if err := copyNativeH2Response(w, resp.Body); err != nil {
			f.captureError(fmt.Errorf("incomplete actual HTTP/2 fixture response: %w", err))
			return
		}
		delivered = true
	})
	server := &http2.Server{}
	server.ServeConn(&prefixConn{Conn: conn, r: pr}, &http2.ServeConnOpts{Handler: handler})
	_ = pr.Close()
	_ = conn.Close()
	err := <-parsed
	var end *h2ReadEnd
	if errors.As(err, &end) {
		f.mu.Lock()
		if f.h2ReadEnds == nil {
			f.h2ReadEnds = map[int]*h2ReadEnd{}
		}
		f.h2ReadEnds[id] = end
		f.mu.Unlock()
		if end.CleanPeerClose {
			return
		}
	}
	if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
		f.captureError(fmt.Errorf("HTTP/2 frame observation: %w", err))
	}
}

// The HTTP/1 pipe is not delivery evidence. Mark completion only after the
// actual HTTP/2 response writer accepts every body byte and FlushError succeeds.
func copyNativeH2Response(w http.ResponseWriter, body io.Reader) error {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	defer controller.SetWriteDeadline(time.Time{})
	if _, err := io.Copy(w, body); err != nil {
		return err
	}
	return controller.Flush()
}
