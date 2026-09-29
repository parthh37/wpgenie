package cluster

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Pair enrols a node. It connects to address, checks that the server there
// holds the key the pairing code names (so a machine in the middle, or the
// wrong server, is refused before anything is sent), issues the node's
// certificate for that key and hands it over with the one-time secret.
// It returns what the node reports about itself and when its certificate
// expires; the node's key is the pairing code's KeyHash (pin it).
func Pair(ctx context.Context, ca *CA, address string, code PairingCode, id string, num int64) (json.RawMessage, time.Time, error) {
	if !ValidNodeID(id) {
		return nil, time.Time{}, fmt.Errorf("invalid node ID %q", id)
	}
	pinned := &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The chain is self-signed while unpaired: trust comes from the
		// pinned key alone, checked below.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			h, err := KeyHash(cs.PeerCertificates[0].PublicKey)
			if err != nil {
				return err
			}
			if !secretEqual(h[:], code.KeyHash[:]) {
				return errors.New("the server's key doesn't match the pairing code (wrong address, or someone in between)")
			}
			return nil
		},
		NextProtos: []string{"h2", "http/1.1"},
	}
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: pinned}
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("connecting to %s: %w", address, err)
	}
	leaf := conn.(*tls.Conn).ConnectionState().PeerCertificates[0]
	conn.Close()

	certPEM, notAfter, err := ca.Issue(leaf.PublicKey, id, false)
	if err != nil {
		return nil, time.Time{}, err
	}
	body, _ := json.Marshal(pairRequest{
		Secret: hex.EncodeToString(code.Secret[:]), NodeID: id, Num: num,
		CA: string(ca.PEM()), Cert: string(certPEM),
	})
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: pinned, ForceAttemptHTTP2: true,
		DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+address+"/cluster/v1/pair", bytes.NewReader(body))
	if err != nil {
		return nil, time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, time.Time{}, &StatusError{Code: resp.StatusCode, Msg: string(out)}
	}
	return out, notAfter, nil
}

// Renew reissues a node's certificate for the key it already has and
// sends it over the existing mutual-TLS channel.
func (c *Client) Renew(ctx context.Context, ca *CA, n Endpoint) (time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+n.Address+"/cluster/v1/info", nil)
	if err != nil {
		return time.Time{}, err
	}
	resp, err := c.Do(n, req)
	if err != nil {
		return time.Time{}, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		return time.Time{}, errors.New("no peer certificate")
	}
	certPEM, notAfter, err := ca.Issue(resp.TLS.PeerCertificates[0].PublicKey, n.ID, false)
	if err != nil {
		return time.Time{}, err
	}
	return notAfter, c.Call(ctx, n, http.MethodPost, "/cluster/v1/renew", renewRequest{Cert: string(certPEM)}, nil)
}

// PeerCertExpiry is when the certificate a node presented expires.
func PeerCertExpiry(cs *tls.ConnectionState) time.Time {
	if cs == nil || len(cs.PeerCertificates) == 0 {
		return time.Time{}
	}
	return cs.PeerCertificates[0].NotAfter
}
