package api

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" // CloudFront's RSA signed-URL protocol requires SHA-1.
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type photoCDN struct {
	base, keyID string
	key         *rsa.PrivateKey
}

func newPhotoCDN(base, id, privatePEM string) (*photoCDN, error) {
	if base == "" && id == "" && privatePEM == "" {
		return nil, nil
	}
	invalid := errors.New("invalid CloudFront configuration: HTTPS base URL, public key ID and RSA-2048 private PEM are required")
	u, e := url.Parse(base)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || id == "" {
		return nil, invalid
	}
	block, _ := pem.Decode([]byte(privatePEM))
	if block == nil {
		return nil, invalid
	}
	var key *rsa.PrivateKey
	if block.Type == "RSA PRIVATE KEY" {
		key, e = x509.ParsePKCS1PrivateKey(block.Bytes)
	} else {
		var k any
		k, e = x509.ParsePKCS8PrivateKey(block.Bytes)
		key, _ = k.(*rsa.PrivateKey)
	}
	if e != nil || key == nil || key.N.BitLen() != 2048 || key.Validate() != nil {
		return nil, invalid
	}
	return &photoCDN{strings.TrimRight(base, "/"), id, key}, nil
}
func cloudFrontBase64(b []byte) string {
	return strings.NewReplacer("+", "-", "=", "_", "/", "~").Replace(base64.StdEncoding.EncodeToString(b))
}
func (c *photoCDN) signedURL(path string, expires time.Time) (string, error) {
	resource := c.base + "/photos/" + path
	// Canned policy must match CloudFront's exact serialization/order.
	policy := struct {
		Statement []struct {
			Resource  string
			Condition struct{ DateLessThan map[string]int64 }
		}
	}{}
	policy.Statement = append(policy.Statement, struct {
		Resource  string
		Condition struct{ DateLessThan map[string]int64 }
	}{Resource: resource})
	policy.Statement[0].Condition.DateLessThan = map[string]int64{"AWS:EpochTime": expires.Unix()}
	b, e := json.Marshal(policy)
	if e != nil {
		return "", e
	}
	digest := sha1.Sum(b)
	signature, e := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA1, digest[:])
	if e != nil {
		return "", e
	}
	q := url.Values{"Expires": {strconv.FormatInt(expires.Unix(), 10)}, "Signature": {cloudFrontBase64(signature)}, "Key-Pair-Id": {c.keyID}}
	return resource + "?" + q.Encode(), nil
}
