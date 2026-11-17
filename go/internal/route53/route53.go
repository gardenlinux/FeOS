// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package route53 writes the TXT records of the ACME zone in Amazon Route
// 53 (ADR 0045): the first DNS provider of a wildcard's DNS-01
// challenges. One call of the API, ChangeResourceRecordSets, signed with
// SigV4 (internal/awsauth) with the credentials the platform hands the
// process: static keys or an EKS Pod Identity. The credential needs
// route53:ChangeResourceRecordSets on the ACME zone's hosted zone alone.
// Standard library only.
package route53

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/awsauth"
)

// Route 53's API is global: its requests are signed for us-east-1.
const (
	endpoint = "https://route53.amazonaws.com"
	region   = "us-east-1"
	service  = "route53"
	// ttl is a challenge record's TTL: the CA's resolvers ask once, and
	// a record of an earlier order is gone within a minute.
	ttl = 60
)

// Credentials hands out the credentials a request is signed with
// (*awsauth.Source).
type Credentials interface {
	Get(ctx context.Context) (awsauth.Credentials, error)
}

// Zone is one hosted zone of Route 53.
type Zone struct {
	id       string
	creds    Credentials
	client   *http.Client
	endpoint string
	now      func() time.Time
}

// New returns the hosted zone id (Z…, with or without its /hostedzone/
// prefix), written with the credentials of creds.
func New(id string, creds Credentials) (*Zone, error) {
	id = strings.TrimPrefix(strings.TrimSpace(id), "/hostedzone/")
	if id == "" || strings.ContainsAny(id, "/?#") {
		return nil, fmt.Errorf("route53: %q is not a hosted zone's ID", id)
	}
	if creds == nil {
		return nil, errors.New("route53: no credentials")
	}
	return &Zone{id: id, creds: creds, client: &http.Client{Timeout: 30 * time.Second}, endpoint: endpoint,
		now: time.Now}, nil
}

// SetTXT makes value the one TXT record at name (UPSERT).
func (z *Zone) SetTXT(ctx context.Context, name, value string) error {
	return z.change(ctx, "UPSERT", name, value)
}

// DeleteTXT deletes the TXT record value at name; a record that is not
// there is no error.
func (z *Zone) DeleteTXT(ctx context.Context, name, value string) error {
	err := z.change(ctx, "DELETE", name, value)
	var ae *apiError
	if errors.As(err, &ae) && ae.code == "InvalidChangeBatch" && strings.Contains(ae.msg, "not found") {
		return nil
	}
	return err
}

// The request of ChangeResourceRecordSets with one change of one TXT
// record.
type changeRequest struct {
	XMLName xml.Name `xml:"https://route53.amazonaws.com/doc/2013-04-01/ ChangeResourceRecordSetsRequest"`
	Batch   struct {
		Comment string   `xml:"Comment"`
		Changes []change `xml:"Changes>Change"`
	} `xml:"ChangeBatch"`
}

type change struct {
	Action string `xml:"Action"`
	Set    struct {
		Name    string   `xml:"Name"`
		Type    string   `xml:"Type"`
		TTL     int      `xml:"TTL"`
		Records []string `xml:"ResourceRecords>ResourceRecord>Value"`
	} `xml:"ResourceRecordSet"`
}

// apiError is an error answer of the API: its code and message.
type apiError struct {
	status    int
	code, msg string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("route53: %d %s: %s", e.status, e.code, e.msg)
}

// change sends one change of the TXT record value at name.
func (z *Zone) change(ctx context.Context, action, name, value string) error {
	var req changeRequest
	req.Batch.Comment = "ferund: ACME DNS-01"
	c := change{Action: action}
	c.Set.Name, c.Set.Type, c.Set.TTL = strings.TrimSuffix(name, ".")+".", "TXT", ttl
	c.Set.Records = []string{quote(value)}
	req.Batch.Changes = []change{c}
	body, err := xml.Marshal(req)
	if err != nil {
		return fmt.Errorf("route53: %w", err)
	}
	body = append([]byte(xml.Header), body...)
	creds, err := z.creds.Get(ctx)
	if err != nil {
		return fmt.Errorf("route53: %w", err)
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, z.endpoint+"/2013-04-01/hostedzone/"+z.id+"/rrset/",
		bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("route53: %w", err)
	}
	hr.Header.Set("Content-Type", "text/xml")
	awsauth.Sign(hr, body, creds, region, service, z.now())
	resp, err := z.client.Do(hr)
	if err != nil {
		return fmt.Errorf("route53: %s %s: %w", action, name, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return parseError(resp.StatusCode, out)
}

// parseError reads an error answer: an ErrorResponse, or the
// InvalidChangeBatch answer with its messages.
func parseError(status int, body []byte) error {
	var er struct {
		Error struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
	}
	var icb struct {
		XMLName  xml.Name
		Messages []string `xml:"Messages>Message"`
	}
	switch {
	case xml.Unmarshal(body, &er) == nil && er.Error.Code != "":
		return &apiError{status: status, code: er.Error.Code, msg: er.Error.Message}
	case xml.Unmarshal(body, &icb) == nil && icb.XMLName.Local == "InvalidChangeBatch":
		return &apiError{status: status, code: "InvalidChangeBatch", msg: strings.Join(icb.Messages, "; ")}
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 512 {
		text = text[:512] + "…"
	}
	return &apiError{status: status, code: http.StatusText(status), msg: text}
}

// quote is a TXT record's value as Route 53 takes it: one string in
// quotes, its quotes and backslashes escaped.
func quote(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
}
