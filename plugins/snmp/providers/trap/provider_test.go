package trap_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gosnmp/gosnmp"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/snmp"
	"github.com/can3p/tommy/plugins/snmp/providers/trap"
)

// startListener boots a whole tommy with this provider on an ephemeral port,
// so parallel test runs never collide, and returns the address it bound.
func startListener(t *testing.T) (*testutil.Instance, string) {
	t.Helper()
	prov := trap.New()
	cfg := config.Ephemeral()
	cfg.SetProvider(snmp.Name, trap.ProviderName, config.NewProviderConfig(map[string]any{"port": 0}))

	inst := testutil.Start(t, cfg, snmp.New(prov))
	addr, err := prov.Addr(5 * time.Second)
	if err != nil {
		t.Fatalf("listener never bound: %v", err)
	}
	return inst, addr
}

// client connects a real gosnmp client to addr - the vendor's own SDK, not a
// hand-rolled driver, per docs/lessons.md ("drive the official SDK, not just
// the wire format").
func client(t *testing.T, addr string, version gosnmp.SnmpVersion, community string) *gosnmp.GoSNMP {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	g := &gosnmp.GoSNMP{
		Target:    host,
		Port:      uint16(port), //nolint:gosec
		Community: community,
		Version:   version,
		Timeout:   3 * time.Second,
		Retries:   1,
	}
	if err := g.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = g.Conn.Close() })
	return g
}

func waitForTrap(t *testing.T, inst *testutil.Instance) (*event.Event, *snmp.Trap) {
	t.Helper()
	events := inst.WaitForEvents(1, store.Query{Plugin: snmp.Name}, 5*time.Second)
	e := events[0]
	tr, ok := snmp.TrapOf(e)
	if !ok {
		t.Fatalf("event %s carries no snmp trap: %+v", e.ID, e.Payload)
	}
	return e, tr
}

// TestReceiveV1Trap sends a real SNMPv1 trap with gosnmp's own client and
// asserts the canonical model recorded, including the header fields (agent
// address, generic/specific trap, enterprise OID) that only a v1 trap
// carries - and that no reply was made, since v1 traps are unconfirmed.
func TestReceiveV1Trap(t *testing.T) {
	inst, addr := startListener(t)
	g := client(t, addr, gosnmp.Version1, "public")

	sent := gosnmp.SnmpTrap{
		Enterprise:   "1.3.6.1.4.1.8072.3.2.10",
		AgentAddress: "192.0.2.1",
		GenericTrap:  3, // linkUp
		SpecificTrap: 0,
		Timestamp:    4200,
		Variables: []gosnmp.SnmpPDU{
			{Name: "1.3.6.1.2.1.2.2.1.1.1", Type: gosnmp.Integer, Value: 1},
		},
	}
	if _, err := g.SendTrap(sent); err != nil {
		t.Fatalf("SendTrap: %v", err)
	}

	e, tr := waitForTrap(t, inst)

	if tr.Version != snmp.VersionV1 {
		t.Errorf("Version = %q, want 1", tr.Version)
	}
	if tr.Inform {
		t.Error("Inform = true for a v1 trap")
	}
	if tr.Community != "public" {
		t.Errorf("Community = %q", tr.Community)
	}
	if tr.V2 != nil {
		t.Errorf("V2 = %+v, want nil for a v1 trap", tr.V2)
	}
	if tr.V1 == nil {
		t.Fatal("V1 = nil")
	}
	if got := strings.TrimPrefix(tr.V1.EnterpriseOID, "."); got != "1.3.6.1.4.1.8072.3.2.10" {
		t.Errorf("EnterpriseOID = %q", tr.V1.EnterpriseOID)
	}
	if tr.V1.AgentAddress != "192.0.2.1" {
		t.Errorf("AgentAddress = %q", tr.V1.AgentAddress)
	}
	if tr.V1.GenericTrap != 3 || tr.V1.GenericTrapName != "linkUp" {
		t.Errorf("GenericTrap = %d %q, want 3 linkUp", tr.V1.GenericTrap, tr.V1.GenericTrapName)
	}
	if tr.V1.Timestamp != 4200 {
		t.Errorf("Timestamp = %d, want 4200", tr.V1.Timestamp)
	}
	if len(tr.Varbinds) != 1 || tr.Varbinds[0].Value != "1" {
		t.Errorf("Varbinds = %+v", tr.Varbinds)
	}

	if e.Raw.Transport != "udp" {
		t.Errorf("Raw.Transport = %q, want udp", e.Raw.Transport)
	}
	if e.Raw.Text {
		t.Error("Raw.Text = true, want false")
	}
	if e.Raw.PeerAddr == "" {
		t.Error("Raw.PeerAddr is empty")
	}
	if len(e.Raw.Body) == 0 {
		t.Error("Raw.Body is empty")
	}
	if e.Provider != trap.ProviderName {
		t.Errorf("Provider = %q, want %q", e.Provider, trap.ProviderName)
	}

	// A v1 trap is unconfirmed: proving no reply arrived means proving
	// nothing showed up on the client's own connection within a short
	// window.
	assertNoReply(t, g)
}

// TestReceiveV2cTrap sends a real SNMPv2c trap (sysUpTime.0 and
// snmpTrapOID.0 leading the varbind list, per RFC 3416) and asserts the v2c
// shape, distinct from v1's, and that - like v1 - it gets no reply.
func TestReceiveV2cTrap(t *testing.T) {
	inst, addr := startListener(t)
	g := client(t, addr, gosnmp.Version2c, "public")

	trapOID := gosnmp.SnmpPDU{Name: "1.3.6.1.6.3.1.1.4.1.0", Type: gosnmp.ObjectIdentifier, Value: "1.3.6.1.6.3.1.1.5.3"}
	sysDescr := gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.1.5.0", Type: gosnmp.OctetString, Value: []byte("host01")}
	if _, err := g.SendTrap(gosnmp.SnmpTrap{Variables: []gosnmp.SnmpPDU{trapOID, sysDescr}}); err != nil {
		t.Fatalf("SendTrap: %v", err)
	}

	e, tr := waitForTrap(t, inst)

	if tr.Version != snmp.VersionV2c {
		t.Errorf("Version = %q, want 2c", tr.Version)
	}
	if tr.Inform {
		t.Error("Inform = true for an unconfirmed trap")
	}
	if tr.V1 != nil {
		t.Errorf("V1 = %+v, want nil for a v2c trap", tr.V1)
	}
	if tr.V2 == nil {
		t.Fatal("V2 = nil")
	}
	if got := strings.TrimPrefix(tr.V2.TrapOID, "."); got != "1.3.6.1.6.3.1.1.5.3" {
		t.Errorf("TrapOID = %q", tr.V2.TrapOID)
	}
	// SendTrap auto-prepends a TimeTicks sysUpTime.0 varbind when the first
	// one given is not already TimeTicks (see gosnmp's trap.go), which is
	// exactly what a real sender does - so 3 varbinds arrive, not 2.
	if len(tr.Varbinds) != 3 {
		t.Fatalf("Varbinds = %+v, want 3 (auto sysUpTime + the 2 given)", tr.Varbinds)
	}
	if tr.Varbinds[2].Value != "host01" || tr.Varbinds[2].Binary {
		t.Errorf("third varbind = %+v, want the printable sysDescr", tr.Varbinds[2])
	}
	if e.Meta["community"] != "public" {
		t.Errorf("Meta[community] = %v", e.Meta["community"])
	}

	assertNoReply(t, g)
}

// TestReceiveInformGetsResponse is the one case that needs a reply: gosnmp's
// own SendTrap blocks waiting for it when IsInform is set, and a non-error
// return is proof the GetResponse it built round-tripped correctly -
// gosnmp's client-side code already discards anything that doesn't answer
// the request it just sent, so this exercises the request-id correlation
// too, not just that some UDP packet came back.
func TestReceiveInformGetsResponse(t *testing.T) {
	inst, addr := startListener(t)
	g := client(t, addr, gosnmp.Version2c, "public")

	trapOID := gosnmp.SnmpPDU{Name: "1.3.6.1.6.3.1.1.4.1.0", Type: gosnmp.ObjectIdentifier, Value: "1.3.6.1.6.3.1.1.5.1"}
	resp, err := g.SendTrap(gosnmp.SnmpTrap{Variables: []gosnmp.SnmpPDU{trapOID}, IsInform: true})
	if err != nil {
		t.Fatalf("SendTrap(inform): %v, want the GetResponse tommy sends back", err)
	}
	if resp.PDUType != gosnmp.GetResponse {
		t.Errorf("response PDUType = %v, want GetResponse", resp.PDUType)
	}
	if resp.Error != gosnmp.NoError {
		t.Errorf("response Error = %v, want NoError", resp.Error)
	}
	// The varbinds must be echoed back unchanged (RFC 3416 §4.2.7): the
	// auto-injected sysUpTime.0 plus the trap OID given.
	if len(resp.Variables) != 2 {
		t.Fatalf("response Variables = %+v, want 2 echoed back", resp.Variables)
	}
	// gosnmp always renders a decoded OID with a leading dot (see
	// decode.go's package comment), and this varbind made a full round trip
	// through tommy's decode-then-reencode - so it comes back dotted even
	// though it was sent without one.
	if got := strings.TrimPrefix(resp.Variables[1].Name, "."); got != "1.3.6.1.6.3.1.1.4.1.0" {
		t.Errorf("response Variables[1].Name = %q", resp.Variables[1].Name)
	}

	_, tr := waitForTrap(t, inst)
	if !tr.Inform {
		t.Error("Inform = false, want true for an InformRequest PDU")
	}
	if tr.Version != snmp.VersionV2c {
		t.Errorf("Version = %q, want 2c", tr.Version)
	}
}

// TestCommunityAcceptedRegardlessOfValue proves CLAUDE.md rule 1: any
// community string is accepted and recorded, never rejected - there is no
// config knob that pins one, unlike SMTP's or FTP's credentials, because
// SNMP community strings are not meaningfully "wrong" in a test fake the way
// a mismatched password is.
func TestCommunityAcceptedRegardlessOfValue(t *testing.T) {
	inst, addr := startListener(t)
	g := client(t, addr, gosnmp.Version2c, "not-the-real-community-at-all")

	trapOID := gosnmp.SnmpPDU{Name: "1.3.6.1.6.3.1.1.4.1.0", Type: gosnmp.ObjectIdentifier, Value: "1.3.6.1.6.3.1.1.5.1"}
	if _, err := g.SendTrap(gosnmp.SnmpTrap{Variables: []gosnmp.SnmpPDU{trapOID}}); err != nil {
		t.Fatalf("SendTrap: %v", err)
	}

	_, tr := waitForTrap(t, inst)
	if tr.Community != "not-the-real-community-at-all" {
		t.Errorf("Community = %q, want it recorded verbatim", tr.Community)
	}
}

// TestGarbageDatagramStillRecorded proves a datagram that is not valid SNMP
// at all is still captured with a DecodeError, rather than silently
// dropped - and, since it does not go through gosnmp's client (which would
// only ever construct well-formed packets), this is the lessons.md case of
// testing hostile input by dropping below the client library.
func TestGarbageDatagramStillRecorded(t *testing.T) {
	inst, addr := startListener(t)

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("this is not an SNMP packet at all")); err != nil {
		t.Fatalf("write: %v", err)
	}

	events := inst.WaitForEvents(1, store.Query{Plugin: snmp.Name}, 5*time.Second)
	tr, ok := snmp.TrapOf(events[0])
	if !ok {
		t.Fatalf("event carries no snmp trap: %+v", events[0].Payload)
	}
	if tr.DecodeError == "" {
		t.Error("DecodeError is empty, want a reason the datagram could not be parsed")
	}
	if tr.Version != "" || len(tr.Varbinds) != 0 {
		t.Errorf("tr = %+v, want an empty model alongside the decode error", tr)
	}
	if string(events[0].Raw.Body) != "this is not an SNMP packet at all" {
		t.Errorf("Raw.Body = %q, want the untouched garbage preserved", events[0].Raw.Body)
	}
}

// TestListenStopsOnContextCancel proves the provider honors the lifecycle
// the core supervises it with.
func TestListenStopsOnContextCancel(t *testing.T) {
	prov := trap.New()
	d := plugin.Deps{Config: config.NewProviderConfig(map[string]any{"port": 0})}.Normalize()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- prov.Listen(ctx, d) }()

	if _, err := prov.Addr(5 * time.Second); err != nil {
		t.Fatalf("listener never bound: %v", err)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Listen returned %v, want nil on a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not return after the context was canceled")
	}
}

// TestGenericViewRendersHostileOctetStringInert proves the security
// invariant holds even though this plugin ships no bespoke UI: an
// OctetString is attacker-controlled and may contain markup, and the
// generic event view (which is all this plugin has) must still interpolate
// it as inert text. Asserted against parsed markup, not a substring search -
// docs/lessons.md is explicit that grepping for "<script" proves nothing.
func TestGenericViewRendersHostileOctetStringInert(t *testing.T) {
	inst, addr := startListener(t)
	g := client(t, addr, gosnmp.Version2c, "public")

	hostile := `<script>window.pwned=true</script><img src=x onerror=alert(1)>`
	trapOID := gosnmp.SnmpPDU{Name: "1.3.6.1.6.3.1.1.4.1.0", Type: gosnmp.ObjectIdentifier, Value: "1.3.6.1.6.3.1.1.5.1"}
	payload := gosnmp.SnmpPDU{Name: "1.3.6.1.2.1.1.5.0", Type: gosnmp.OctetString, Value: []byte(hostile)}
	if _, err := g.SendTrap(gosnmp.SnmpTrap{Variables: []gosnmp.SnmpPDU{trapOID, payload}}); err != nil {
		t.Fatalf("SendTrap: %v", err)
	}

	events := inst.WaitForEvents(1, store.Query{Plugin: snmp.Name}, 5*time.Second)
	id := events[0].ID

	// Fetched as an htmx fragment (HX-Request: true), not the full shell
	// page: the full page legitimately carries the vendored htmx <script>
	// tags, which would make a bare "does the page contain <script>" check
	// meaningless. The fragment is exactly what the detail pane swaps in.
	req, err := http.NewRequest(http.MethodGet, inst.UI("snmp/events/"+string(id)), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("HX-Request", "true")
	resp := inst.Do(req)
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("GET snmp event detail: status = %d, body = %s", resp.StatusCode, bodyBytes)
	}
	body := string(bodyBytes)

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse rendered fragment: %v", err)
	}
	if doc.Find("script").Length() != 0 {
		t.Error("rendered fragment contains a <script> element - hostile varbind text was not escaped")
	}
	if _, ok := doc.Find("[onerror]").Attr("onerror"); ok {
		t.Error("rendered fragment carries an onerror attribute - hostile varbind text was not escaped")
	}
	if !strings.Contains(body, "window.pwned") {
		t.Error("the escaped text of the hostile value is not present at all - the value itself should still be visible, just inert")
	}
}

// TestNetSNMPSnmptrapBinary is a bonus over the gosnmp-client tests above: it
// drives net-snmp's own snmptrap command-line tool, a completely independent
// implementation, so a mismatch here could not be explained by tommy and
// gosnmp sharing a misunderstanding of the wire format. Skipped when the
// binary is not on PATH rather than failing, since it is not installed
// everywhere CI might run.
func TestNetSNMPSnmptrapBinary(t *testing.T) {
	bin, err := exec.LookPath("snmptrap")
	if err != nil {
		t.Skip("snmptrap not found on PATH")
	}

	inst, addr := startListener(t)

	cmd := exec.Command(bin, "-v", "2c", "-c", "public", addr, "",
		"1.3.6.1.6.3.1.1.5.3", "1.3.6.1.2.1.1.5.0", "s", "host01")
	// Net-SNMP tries to write a persistent-state directory and load MIBs by
	// default; neither is available or needed in a test sandbox, and both
	// are harmless warnings on stderr rather than failures.
	cmd.Env = append(cmd.Environ(), "MIBS=", "MIBDIRS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("snmptrap: %v\n%s", err, out)
	}

	_, tr := waitForTrap(t, inst)
	if tr.Version != snmp.VersionV2c {
		t.Errorf("Version = %q, want 2c", tr.Version)
	}
	if got := strings.TrimPrefix(tr.V2.TrapOID, "."); got != "1.3.6.1.6.3.1.1.5.3" {
		t.Errorf("TrapOID = %q", tr.V2.TrapOID)
	}
	found := false
	for _, vb := range tr.Varbinds {
		if vb.Value == "host01" {
			found = true
		}
	}
	if !found {
		t.Errorf("Varbinds = %+v, want the sysDescr varbind snmptrap sent", tr.Varbinds)
	}
}

func assertNoReply(t *testing.T, g *gosnmp.GoSNMP) {
	t.Helper()
	if err := g.Conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 512)
	n, err := g.Conn.Read(buf)
	if err == nil {
		t.Fatalf("unexpected reply of %d bytes: %x", n, bytes.TrimRight(buf[:n], "\x00"))
	}
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("Read() error = %v, want a read timeout (no reply)", err)
	}
}
