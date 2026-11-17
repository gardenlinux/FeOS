// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/awsauth"
)

// An MSK broker's name, the region in it.
const broker = "b-1.ferun-log.abc123.c2.kafka.eu-west-1.amazonaws.com:9098"

// Without AWS credentials a client signs in with nothing (plaintext); with
// them over TLS with SASL.
func TestAuthOpts(t *testing.T) {
	if n := len(Auth{}.opts()); n != 0 {
		t.Errorf("plaintext has %d options", n)
	}
	get := func(context.Context) (awsauth.Credentials, error) { return awsauth.Credentials{}, nil }
	if n := len((Auth{AWS: get}).opts()); n != 2 {
		t.Errorf("AWS_MSK_IAM has %d options, want TLS and SASL", n)
	}
	for _, a := range []Auth{{}, {AWS: get}} {
		c, err := New([]string{broker}, a)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
}

// Every session signs its challenge with the credentials current when it
// starts, so that rotated ones are taken up; a failed fetch fails the
// session.
func TestMSKIAM(t *testing.T) {
	creds := []awsauth.Credentials{
		{AccessKeyID: "AKIAFIRST", SecretAccessKey: "s1", SessionToken: "tok1"},
		{AccessKeyID: "AKIASECOND", SecretAccessKey: "s2"},
	}
	calls := 0
	m := mskIAM(func(context.Context) (awsauth.Credentials, error) {
		if calls == len(creds) {
			return awsauth.Credentials{}, errors.New("no credentials")
		}
		calls++
		return creds[calls-1], nil
	})
	if m.Name() != "AWS_MSK_IAM" {
		t.Errorf("mechanism %s", m.Name())
	}
	for i, want := range creds {
		_, msg, err := m.Authenticate(context.Background(), broker)
		if err != nil {
			t.Fatal(err)
		}
		var ch map[string]string
		if err := json.Unmarshal(msg, &ch); err != nil {
			t.Fatalf("challenge %d: %v", i, err)
		}
		if !strings.HasPrefix(ch["x-amz-credential"], want.AccessKeyID+"/") ||
			!strings.Contains(ch["x-amz-credential"], "/eu-west-1/kafka-cluster/") ||
			ch["x-amz-security-token"] != want.SessionToken || ch["action"] != "kafka-cluster:Connect" ||
			ch["host"] != strings.TrimSuffix(broker, ":9098") {
			t.Errorf("challenge %d: %v", i, ch)
		}
	}
	if _, _, err := m.Authenticate(context.Background(), broker); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Errorf("a failed fetch: %v", err)
	}
}
