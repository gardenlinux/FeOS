// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package route53

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/awsauth"
)

type staticCreds struct{}

func (staticCreds) Get(context.Context) (awsauth.Credentials, error) {
	return awsauth.Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET"}, nil
}

// fakeAPI is Route 53's rrset endpoint: it records the changes it got
// and answers what the test sets.
type fakeAPI struct {
	mu      sync.Mutex
	got     []changeRequest
	paths   []string
	auth    []string
	status  int
	answer  string
	present map[string]bool
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var req changeRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.got = append(f.got, req)
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	c := req.Batch.Changes[0]
	if c.Action == "DELETE" && !f.present[c.Set.Name] {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<InvalidChangeBatch xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><Messages><Message>Tried to delete resource record set [name='`+
			c.Set.Name+`', type='TXT'] but it was not found</Message></Messages></InvalidChangeBatch>`)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.answer)
		return
	}
	if c.Action == "UPSERT" {
		f.present[c.Set.Name] = true
	} else {
		delete(f.present, c.Set.Name)
	}
	_, _ = io.WriteString(w, `<?xml version="1.0"?><ChangeResourceRecordSetsResponse><ChangeInfo><Status>PENDING</Status></ChangeInfo></ChangeResourceRecordSetsResponse>`)
}

func newZone(t *testing.T) (*Zone, *fakeAPI) {
	t.Helper()
	f := &fakeAPI{present: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	z, err := New("/hostedzone/Z0ACME", staticCreds{})
	if err != nil {
		t.Fatal(err)
	}
	z.endpoint, z.client = srv.URL, srv.Client()
	z.now = func() time.Time { return time.Date(2026, 11, 17, 20, 0, 0, 0, time.UTC) }
	return z, f
}

// SetTXT upserts one quoted TXT record with a short TTL on the zone's
// rrset, signed for route53 in us-east-1; DeleteTXT deletes it, and a
// record that is gone already is no error.
func TestSetAndDeleteTXT(t *testing.T) {
	z, f := newZone(t)
	name := "abcd.acme.fecore.test"
	if err := z.SetTXT(context.Background(), name, "tok-en_1"); err != nil {
		t.Fatal(err)
	}
	if err := z.DeleteTXT(context.Background(), name, "tok-en_1"); err != nil {
		t.Fatal(err)
	}
	if err := z.DeleteTXT(context.Background(), name, "tok-en_1"); err != nil {
		t.Fatalf("a record not there: %v", err)
	}
	if len(f.got) != 3 {
		t.Fatalf("%d calls", len(f.got))
	}
	c := f.got[0].Batch.Changes[0]
	if c.Action != "UPSERT" || c.Set.Name != name+"." || c.Set.Type != "TXT" || c.Set.TTL != 60 ||
		len(c.Set.Records) != 1 || c.Set.Records[0] != `"tok-en_1"` {
		t.Fatalf("change %+v", c)
	}
	if f.got[1].Batch.Changes[0].Action != "DELETE" {
		t.Fatalf("second change %+v", f.got[1].Batch.Changes[0])
	}
	if f.paths[0] != "POST /2013-04-01/hostedzone/Z0ACME/rrset/" {
		t.Fatalf("path %s", f.paths[0])
	}
	if !strings.HasPrefix(f.auth[0], "AWS4-HMAC-SHA256 Credential=AKID/20261117/us-east-1/route53/aws4_request,") {
		t.Fatalf("Authorization %s", f.auth[0])
	}
}

// An error answer names its code and message.
func TestErrorAnswer(t *testing.T) {
	z, f := newZone(t)
	f.status = http.StatusForbidden
	f.answer = `<?xml version="1.0"?><ErrorResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>User is not authorized</Message></Error></ErrorResponse>`
	err := z.SetTXT(context.Background(), "x.acme.fecore.test", "v")
	if err == nil || !strings.Contains(err.Error(), "403 AccessDenied: User is not authorized") {
		t.Fatalf("err %v", err)
	}
}

// A zone's ID is Z… with or without its prefix, and nothing else.
func TestNew(t *testing.T) {
	for _, id := range []string{"", "Z1/../x", "a?b"} {
		if _, err := New(id, staticCreds{}); err == nil {
			t.Errorf("%q accepted", id)
		}
	}
	if _, err := New("Z1", nil); err == nil {
		t.Error("no credentials accepted")
	}
	if got := quote(`a"b\c`); got != `"a\"b\\c"` {
		t.Errorf("quote %s", got)
	}
}
