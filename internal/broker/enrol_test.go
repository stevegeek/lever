package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func enrolReq(ticket string, csrPEM []byte) *http.Request {
	body, _ := json.Marshal(EnrolRequest{Ticket: ticket, CSR: string(csrPEM)})
	return httptest.NewRequest("POST", "/enrol", bytes.NewReader(body)) // no client cert
}

func TestEnrolSignsCertForMatchingWorker(t *testing.T) {
	b := New(testConfig(t))
	tk, _ := b.tickets.Issue("worker", b.ticketTTL)
	csr := makeCSRForCN(t, "worker")
	w := httptest.NewRecorder()
	b.handleEnrol(w, enrolReq(tk, csr))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp EnrolResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Cert == "" {
		t.Fatal("empty cert")
	}
}

// THE load-bearing test: a ticket for "worker" + a CSR claiming CN "manager"
// must be rejected, AND the ticket must survive (not be burned) so the real
// worker can still enrol.
func TestEnrolRejectsCNMismatchAndPreservesTicket(t *testing.T) {
	b := New(testConfig(t))
	tk, _ := b.tickets.Issue("worker", b.ticketTTL)
	evil := makeCSRForCN(t, "manager") // wrong CN
	w := httptest.NewRecorder()
	b.handleEnrol(w, enrolReq(tk, evil))
	if w.Code == http.StatusOK {
		t.Fatal("SECURITY: enrol must reject a CSR whose CN != ticket grove")
	}
	// The ticket must still be usable by the legitimate worker.
	good := makeCSRForCN(t, "worker")
	w2 := httptest.NewRecorder()
	b.handleEnrol(w2, enrolReq(tk, good))
	if w2.Code != http.StatusOK {
		t.Fatalf("legit worker enrol after a rejected mismatch: status = %d", w2.Code)
	}
}

func TestEnrolRejectsUnknownTicket(t *testing.T) {
	b := New(testConfig(t))
	csr := makeCSRForCN(t, "worker")
	w := httptest.NewRecorder()
	b.handleEnrol(w, enrolReq("not-a-ticket", csr))
	if w.Code == http.StatusOK {
		t.Fatal("unknown ticket must be rejected")
	}
}

func TestEnrolTicketIsSingleUse(t *testing.T) {
	b := New(testConfig(t))
	tk, _ := b.tickets.Issue("worker", b.ticketTTL)
	csr := makeCSRForCN(t, "worker")
	w := httptest.NewRecorder()
	b.handleEnrol(w, enrolReq(tk, csr))
	if w.Code != http.StatusOK {
		t.Fatalf("first enrol: %d", w.Code)
	}
	w2 := httptest.NewRecorder()
	b.handleEnrol(w2, enrolReq(tk, csr)) // reuse
	if w2.Code == http.StatusOK {
		t.Fatal("ticket must be single-use")
	}
}
