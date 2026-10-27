// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package camera

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A phone only opens its camera on a secure page, so the phone door speaks HTTPS with a certificate made here. It is
// self-signed: the phone warns once and the user continues (it is their own computer). It is kept for a year and made
// again when it is about to expire or when the computer's address is not in it any more.

func loadOrCreateCert(dir string, ips []net.IP) (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if pair, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf, err := x509.ParseCertificate(pair.Certificate[0]); err == nil && time.Until(leaf.NotAfter) > 30*24*time.Hour && covers(leaf, ips) {
			return pair, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "LiveTT (camera do celular)"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  append([]net.IP{net.IPv4(127, 0, 0, 1)}, ips...),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}

func covers(leaf *x509.Certificate, ips []net.IP) bool {
	for _, ip := range ips {
		found := false
		for _, have := range leaf.IPAddresses {
			if have.Equal(ip) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// localAddresses lists this computer's private IPv4 addresses, the likeliest Wi-Fi/LAN first. Virtual adapters (VPN,
// virtual machines, WSL, Docker) are skipped: a phone cannot reach them.
func localAddresses() []net.IP {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	type found struct {
		ip   net.IP
		rank int
	}
	var list []found
	for _, nic := range interfaces {
		if nic.Flags&net.FlagUp == 0 || nic.Flags&net.FlagLoopback != 0 {
			continue
		}
		lower := strings.ToLower(nic.Name)
		skip := false
		for _, virtual := range []string{"vethernet", "vmware", "virtualbox", "vbox", "wsl", "docker", "hyper-v", "bluetooth", "tailscale", "zerotier", "vpn", "tap", "tun", "loopback"} {
			if strings.Contains(lower, virtual) {
				skip = true
			}
		}
		if skip {
			continue
		}
		addresses, _ := nic.Addrs()
		for _, a := range addresses {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := n.IP.To4()
			if ip == nil || !ip.IsPrivate() {
				continue
			}
			rank := 2
			switch {
			case ip[0] == 192 && ip[1] == 168:
				rank = 0
			case ip[0] == 10:
				rank = 1
			}
			list = append(list, found{ip, rank})
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].rank < list[j].rank })
	out := make([]net.IP, 0, len(list))
	for _, f := range list {
		out = append(out, f.ip)
	}
	return out
}
