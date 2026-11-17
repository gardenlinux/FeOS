// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package awsauth

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Sign makes the signature of AWS's SigV4 test suite for its get-vanilla
// request.
func TestSignVanilla(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	Sign(req, nil, c, "us-east-1", "service", time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization\n got %s\nwant %s", got, want)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
		t.Fatalf("X-Amz-Date %q", got)
	}
}

// A session token is a header of the request, and signed.
func TestSignSessionToken(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://route53.amazonaws.com/2013-04-01/hostedzone/Z1/rrset/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/xml")
	c := Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET", SessionToken: "TOKEN"}
	Sign(req, []byte("<x/>"), c, "us-east-1", "route53", time.Date(2026, 11, 17, 20, 0, 0, 0, time.UTC))
	if req.Header.Get("X-Amz-Security-Token") != "TOKEN" {
		t.Fatal("no session token header")
	}
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "SignedHeaders=content-type;host;x-amz-date;x-amz-security-token,") ||
		!strings.Contains(auth, "Credential=AKID/20261117/us-east-1/route53/aws4_request,") {
		t.Fatalf("Authorization %s", auth)
	}
}
