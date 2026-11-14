package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"

	"github.com/jxburros/buttercams/internal/config"
)

// Certificate files inside <dataRoot>/certs.
const (
	CertDir  = "certs"
	CertFile = "server.crt"
	KeyFile  = "server.key"
)

// SelfSignedValidity is how long the generated certificate is valid.
const SelfSignedValidity = 10 * 365 * 24 * time.Hour

// CertInfo describes the certificate in use.
type CertInfo struct {
	Subject           string    `json:"subject"`
	NotAfter          time.Time `json:"notAfter"`
	SelfSigned        bool      `json:"selfSigned"`
	DNSNames          []string  `json:"dnsNames"`
	FingerprintSHA256 string    `json:"fingerprintSha256"`
}

func certPaths(dataRoot string) (cert, key string) {
	dir := filepath.Join(dataRoot, CertDir)
	return filepath.Join(dir, CertFile), filepath.Join(dir, KeyFile)
}

// localNames returns the names and addresses this server is reached by.
func localNames() (dns []string, ips []net.IP) {
	seen := map[string]bool{}
	add := func(n string) {
		n = strings.TrimSuffix(strings.TrimSpace(n), ".")
		if n != "" && !seen[strings.ToLower(n)] {
			seen[strings.ToLower(n)] = true
			dns = append(dns, n)
		}
	}
	add("localhost")
	if host, err := os.Hostname(); err == nil {
		add(host)
		if cname, err := net.LookupCNAME(host); err == nil {
			add(cname) // the fully qualified name, where DNS knows it
		}
		if addrs, err := net.LookupHost(host); err == nil {
			for _, a := range addrs {
				if names, err := net.LookupAddr(a); err == nil {
					for _, n := range names {
						if strings.HasPrefix(strings.ToLower(n), strings.ToLower(host)+".") {
							add(n)
						}
					}
				}
			}
		}
	}
	ips = append(ips, net.IPv4(127, 0, 0, 1), net.IPv6loopback)
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				ips = append(ips, ipn.IP)
			}
		}
	}
	return dns, ips
}

// GenerateSelfSigned writes a new self-signed certificate, valid ten years,
// for this server's host name, fully qualified name and IP addresses
// (spec 11.1).
func GenerateSelfSigned(dataRoot string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	if err != nil {
		return err
	}
	dns, ips := localNames()
	cn := "ButterCams"
	if len(dns) > 1 {
		cn = dns[1]
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"ButterCams"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(SelfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return writeCertFiles(dataRoot,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func writeCertFiles(dataRoot string, certPEM, keyPEM []byte) error {
	certPath, keyPath := certPaths(dataRoot)
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return err
	}
	if err := config.WriteFileAtomic(keyPath, keyPEM); err != nil {
		return err
	}
	return config.WriteFileAtomic(certPath, certPEM)
}

// EnsureCertificate generates a self-signed certificate when none exists or
// the existing one cannot be loaded.
func EnsureCertificate(dataRoot string) error {
	certPath, keyPath := certPaths(dataRoot)
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return nil
	}
	return GenerateSelfSigned(dataRoot)
}

// LoadCertificate loads the certificate in use.
func LoadCertificate(dataRoot string) (*tls.Certificate, CertInfo, error) {
	certPath, keyPath := certPaths(dataRoot)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, CertInfo{}, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, CertInfo{}, err
	}
	cert.Leaf = leaf
	return &cert, describeCert(leaf), nil
}

func describeCert(leaf *x509.Certificate) CertInfo {
	sum := sha256.Sum256(leaf.Raw)
	hexSum := strings.ToUpper(hex.EncodeToString(sum[:]))
	var fp []string
	for i := 0; i < len(hexSum); i += 2 {
		fp = append(fp, hexSum[i:i+2])
	}
	names := append([]string{}, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	return CertInfo{
		Subject:           leaf.Subject.String(),
		NotAfter:          leaf.NotAfter,
		SelfSigned:        leaf.Issuer.String() == leaf.Subject.String(),
		DNSNames:          names,
		FingerprintSHA256: strings.Join(fp, ":"),
	}
}

// InstallPEM validates and stores an uploaded PEM certificate and key.
func InstallPEM(dataRoot, certPEM, keyPEM string) error {
	if strings.TrimSpace(certPEM) == "" {
		return FieldErr("certPem", "Paste the certificate in PEM form.")
	}
	if strings.TrimSpace(keyPEM) == "" {
		return FieldErr("keyPem", "Paste the private key in PEM form.")
	}
	if _, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err != nil {
		return FieldErr("certPem", "The certificate and private key do not belong together or cannot be read: "+err.Error())
	}
	return writeCertFiles(dataRoot, []byte(certPEM), []byte(keyPEM))
}

// InstallPFX validates and stores an uploaded PFX (PKCS #12) bundle.
func InstallPFX(dataRoot, pfxBase64, password string) error {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(pfxBase64))
	if err != nil || len(raw) == 0 {
		return FieldErr("pfxBase64", "The PFX file could not be read.")
	}
	key, leaf, chain, err := pkcs12.DecodeChain(raw, password)
	if err != nil {
		if errors.Is(err, pkcs12.ErrIncorrectPassword) {
			return FieldErr("password", "The PFX password is not correct.")
		}
		return FieldErr("pfxBase64", "The PFX file could not be read: "+err.Error())
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return FieldErr("pfxBase64", "The private key in the PFX file is of a type that is not supported.")
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
	for _, c := range chain {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return FieldErr("pfxBase64", fmt.Sprintf("The certificate in the PFX file cannot be used: %v", err))
	}
	return writeCertFiles(dataRoot, certPEM, keyPEM)
}
