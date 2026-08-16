package envelope

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPayloadRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		in      []byte
		textual bool
		wantEnc string
	}{
		{"ascii text", []byte("hello"), true, "u"},
		{"utf-8 text", []byte("你好 · dsh"), true, "u"},
		{"binary marked textual", []byte{0xff, 0xfe, 0x00}, true, "b"},
		{"binary", []byte{0x00, 0x01, 0x02, 0xff}, false, "b"},
		{"empty", []byte{}, true, "u"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPayload(tc.in, tc.textual)
			if p.Enc != tc.wantEnc {
				t.Fatalf("enc = %q, want %q", p.Enc, tc.wantEnc)
			}
			got, err := p.Bytes()
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if string(got) != string(tc.in) {
				t.Fatalf("round trip = %q, want %q", got, tc.in)
			}
		})
	}
}

// Invalid UTF-8 must never ride as "u": the receiver would decode replacement
// characters and silently corrupt the body.
func TestNewPayloadRefusesInvalidUTF8AsText(t *testing.T) {
	p := NewPayload([]byte{0x80, 0x81}, true)
	if p.Enc != "b" {
		t.Fatalf("enc = %q, want b for invalid UTF-8", p.Enc)
	}
}

func TestPayloadBytesRejectsUnknownEncoding(t *testing.T) {
	if _, err := (Payload{Enc: "z", D: "x"}).Bytes(); err == nil {
		t.Fatal("expected an error for an unknown encoding")
	}
}

func TestDiscriminant(t *testing.T) {
	got, err := Discriminant([]byte(`{"t":"head","id":"1","status":200}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != THead {
		t.Fatalf("t = %q, want %q", got, THead)
	}

	if _, err := Discriminant([]byte(`not json`)); err == nil {
		t.Fatal("expected an error for unparseable input")
	}
	if _, err := Discriminant([]byte(`{"id":"1"}`)); err == nil {
		t.Fatal("expected an error for a frame with no discriminant")
	}
}

// The telemetry snapshot must survive verbatim: the control plane stores it as
// jsonb and must never require a field a newer node has added or dropped.
func TestTelemetrySnapshotStaysOpaque(t *testing.T) {
	raw := `{"t":"tlm","ts":1,"snapshot":{"unknownKey":[1,2,3],"nested":{"a":true}}}`
	var f Telemetry
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(string(f.Snapshot), "unknownKey") {
		t.Fatalf("snapshot lost unknown keys: %s", f.Snapshot)
	}
}

// A hello frame must serialise with the field names the TypeScript side sends.
// The two are separate implementations of one document, so a rename on either
// side has to fail here rather than at handshake time on someone's machine.
func TestHelloWireNames(t *testing.T) {
	raw, err := json.Marshal(Hello{
		T: THello, Protocol: ProtocolVersion, NodeID: "n", Token: "tok",
		Node: NodeDescriptor{Platform: "linux", Arch: "x64", DSHVersion: "1", PluginVersion: "2", Cwd: "/w"},
		Caps: []string{"dsh"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"t":"hello"`, `"nodeId":"n"`, `"token":"tok"`, `"dshVersion":"1"`, `"pluginVersion":"2"`, `"caps":["dsh"]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("hello frame missing %s\n  got: %s", want, raw)
		}
	}
}

// An absent body must be omitted rather than sent as null: the node checks for
// undefined.
func TestReqOmitsAbsentBody(t *testing.T) {
	raw, err := json.Marshal(Req{T: TReq, ID: "1", Ns: NsDSH, Method: "GET", Path: "/"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "body") {
		t.Fatalf("absent body should be omitted, got: %s", raw)
	}
}
