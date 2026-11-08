# ADR 0039: Valkey admits a workload by its certificate and by nothing else: TLS 1.3 alone with a certificate of the addon's ID, an ACL user per referencing workload named by its SPIFFE ID, the default user off and no password anywhere; the addon is administered as itself; 7.2 and 8 retire into 9

**Status:** Merged into [ADR 0038](0038-addons-admit-a-workload-by-its-certificate.md) · **Date:** 2026-11-08

It made Valkey the second addon that admits a workload by its
certificate: version 9 alone, 7.2 and 8 retired into it on their
volumes; TLS 1.3 on its one port and no plaintext; one ACL user per
granted workload named by its SPIFFE ID, the default user off and nobody
with a password; the addon administered as its own ID, whose server
certificate is a client's too; a referencing pod's TLS binding offering
no ALPN. All of it is the Valkey section of ADR 0038, whose common rules
(no password anywhere, the map as the grant, provisioning steps) it
shared from the start.
