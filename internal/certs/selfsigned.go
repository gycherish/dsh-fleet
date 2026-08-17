// Package certs mints the self-signed certificate a LAN deployment needs.
//
// This exists because HTTPS is not optional here. Browsers gate
// `crypto.randomUUID`, `crypto.subtle`, service workers and more behind a
// secure context, and a LAN address over plain HTTP is not one — only HTTPS
// and the loopback exemption are. The dsh client calls `crypto.randomUUID`, so
// a phone reaching a control plane over `http://192.168.x.x` gets a broken UI
// while the identical build works from the host over loopback.
//
// A public certificate authority cannot issue for a private address, so a
// self-signed certificate the operator trusts once is the practical answer.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Validity is how long a generated certificate lasts.
//
// 397 days is the ceiling browsers enforce on publicly trusted certificates.
// Self-signed ones are not held to it today, but staying inside it avoids
// depending on that remaining true.
const Validity = 397 * 24 * time.Hour

// Generated reports what was written and what the certificate covers.
type Generated struct {
	CertPath string
	KeyPath  string
	DNSNames []string
	IPs      []string
	NotAfter time.Time
}

// SelfSigned writes a certificate and private key covering hosts.
//
// Every name a browser might type has to be in the SAN list: modern browsers
// ignore the Common Name entirely, so a certificate without the right SAN is
// rejected no matter how it was trusted. Loopback names are always included,
// which keeps local development working against the same file.
//
// @param dir - directory to write cert.pem and key.pem into.
// @param hosts - additional DNS names and IP addresses to cover.
// @returns what was written and what it covers.
func SelfSigned(dir string, hosts []string) (*Generated, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("certs: cannot generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("certs: cannot generate serial: %w", err)
	}

	dnsNames := []string{"localhost"}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			ips = append(ips, ip)
			continue
		}
		if host != "" && host != "localhost" {
			dnsNames = append(dnsNames, host)
		}
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "dsh-fleet", Organization: []string{"dsh-fleet"}},
		// Backdated an hour so a node or phone whose clock runs slightly behind
		// does not reject a certificate minted moments ago.
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(Validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("certs: cannot create certificate: %w", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("certs: cannot create %q: %w", dir, err)
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	if err := writePEM(certPath, 0o644, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("certs: cannot marshal key: %w", err)
	}
	// 0600: the private key is the whole secret.
	if err := writePEM(keyPath, 0o600, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		return nil, err
	}

	printable := make([]string, 0, len(ips))
	for _, ip := range ips {
		printable = append(printable, ip.String())
	}
	return &Generated{
		CertPath: certPath, KeyPath: keyPath,
		DNSNames: dnsNames, IPs: printable, NotAfter: template.NotAfter,
	}, nil
}

// LocalAddresses lists this host's non-loopback IPv4 addresses, so a generated
// certificate covers the address a phone on the same network will actually type.
func LocalAddresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
			continue
		}
		if v4 := ipNet.IP.To4(); v4 != nil {
			out = append(out, v4.String())
		}
	}
	return out
}

func writePEM(path string, mode os.FileMode, block *pem.Block) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("certs: cannot write %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	if err := pem.Encode(file, block); err != nil {
		return fmt.Errorf("certs: cannot encode %q: %w", path, err)
	}
	return file.Close()
}
