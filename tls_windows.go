//go:build windows

package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"fmt"
	"io"
	"log"
	"math/big"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	nCryptDLL            = windows.NewLazySystemDLL("ncrypt.dll")
	procNCryptSignHash   = nCryptDLL.NewProc("NCryptSignHash")
	procNCryptFreeObject = nCryptDLL.NewProc("NCryptFreeObject")

	crypt32DLL                     = windows.NewLazySystemDLL("crypt32.dll")
	procCryptAcquireCertPrivateKey = crypt32DLL.NewProc("CryptAcquireCertificatePrivateKey")
)

const (
	acquireOnlyNCryptKey = 0x00040000
	acquireSilentFlag    = 0x00000040
	ncryptPadPKCS1Flag   = 0x00000002
	ncryptPadPSSFlag     = 0x00000008
)

// winKey is a crypto.Signer backed by a Windows CNG key handle.
type winKey struct {
	handle     uintptr
	pub        crypto.PublicKey
	callerFree bool
}

func (k *winKey) Public() crypto.PublicKey { return k.pub }

func (k *winKey) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	hash := opts.HashFunc()
	algID, err := hashAlgID(hash)
	if err != nil {
		return nil, err
	}

	switch k.pub.(type) {
	case *rsa.PublicKey:
		var paddingInfo unsafe.Pointer
		var flags uint32
		if pss, ok := opts.(*rsa.PSSOptions); ok {
			saltLen := pss.SaltLength
			if saltLen == rsa.PSSSaltLengthAuto || saltLen == 0 {
				saltLen = hash.Size()
			}
			pi := struct {
				pszAlgID *uint16
				cbSalt   uint32
			}{pszAlgID: algID, cbSalt: uint32(saltLen)}
			flags = ncryptPadPSSFlag
			paddingInfo = unsafe.Pointer(&pi)
		} else {
			pi := struct{ pszAlgID *uint16 }{pszAlgID: algID}
			flags = ncryptPadPKCS1Flag
			paddingInfo = unsafe.Pointer(&pi)
		}
		return k.ncryptSign(digest, paddingInfo, flags)

	case *ecdsa.PublicKey:
		raw, err := k.ncryptSign(digest, nil, 0)
		if err != nil {
			return nil, err
		}
		return ecdsaRawToDER(raw)

	default:
		return nil, fmt.Errorf("unsupported key type %T", k.pub)
	}
}

func (k *winKey) ncryptSign(digest []byte, paddingInfo unsafe.Pointer, flags uint32) ([]byte, error) {
	var size uint32
	r, _, _ := procNCryptSignHash.Call(
		k.handle,
		uintptr(paddingInfo),
		uintptr(unsafe.Pointer(&digest[0])),
		uintptr(len(digest)),
		0, 0,
		uintptr(unsafe.Pointer(&size)),
		uintptr(flags),
	)
	if r != 0 {
		return nil, fmt.Errorf("NCryptSignHash (size): 0x%08x", r)
	}
	sig := make([]byte, size)
	r, _, _ = procNCryptSignHash.Call(
		k.handle,
		uintptr(paddingInfo),
		uintptr(unsafe.Pointer(&digest[0])),
		uintptr(len(digest)),
		uintptr(unsafe.Pointer(&sig[0])),
		uintptr(size),
		uintptr(unsafe.Pointer(&size)),
		uintptr(flags),
	)
	if r != 0 {
		return nil, fmt.Errorf("NCryptSignHash: 0x%08x", r)
	}
	return sig[:size], nil
}

func hashAlgID(hash crypto.Hash) (*uint16, error) {
	switch hash {
	case crypto.SHA1:
		return windows.UTF16PtrFromString("SHA1")
	case crypto.SHA256:
		return windows.UTF16PtrFromString("SHA256")
	case crypto.SHA384:
		return windows.UTF16PtrFromString("SHA384")
	case crypto.SHA512:
		return windows.UTF16PtrFromString("SHA512")
	default:
		return nil, fmt.Errorf("unsupported hash %v", hash)
	}
}

func ecdsaRawToDER(raw []byte) ([]byte, error) {
	n := len(raw) / 2
	r := new(big.Int).SetBytes(raw[:n])
	s := new(big.Int).SetBytes(raw[n:])
	return asn1.Marshal(struct{ R, S *big.Int }{r, s})
}

// buildTLSConfig loads a certificate from the Windows Local Machine "MY" store
// and wires up a crypto.Signer via CNG so the private key is never exported.
// certSubject is matched case-insensitively against CN or SAN DNS names;
// leave empty to use the first certificate that has an associated private key.
func buildTLSConfig(certSubject string, logger *log.Logger) (*tls.Config, error) {
	storeName, err := windows.UTF16PtrFromString("MY")
	if err != nil {
		return nil, fmt.Errorf("encode store name: %w", err)
	}

	store, err := windows.CertOpenStore(
		windows.CERT_STORE_PROV_SYSTEM,
		0,
		0,
		windows.CERT_SYSTEM_STORE_LOCAL_MACHINE,
		uintptr(unsafe.Pointer(storeName)),
	)
	if err != nil {
		return nil, fmt.Errorf("open Local Machine MY store: %w", err)
	}
	defer windows.CertCloseStore(store, 0)

	var prev *windows.CertContext
	for {
		ctx, err := windows.CertEnumCertificatesInStore(store, prev)
		if err != nil || ctx == nil {
			break
		}
		prev = ctx

		derBytes := append([]byte(nil), unsafe.Slice(ctx.EncodedCert, ctx.Length)...)
		cert, err := x509.ParseCertificate(derBytes)
		if err != nil {
			continue
		}

		if certSubject != "" {
			needle := strings.ToLower(certSubject)
			matched := strings.Contains(strings.ToLower(cert.Subject.CommonName), needle)
			if !matched {
				for _, san := range cert.DNSNames {
					if strings.Contains(strings.ToLower(san), needle) {
						matched = true
						break
					}
				}
			}
			if !matched {
				continue
			}
		}

		// Try to acquire the private key — skip certs that don't have one.
		key, err := acquirePrivateKey(ctx, cert)
		if err != nil {
			logger.Printf("TLS: skipping cert CN=%s (%v)", cert.Subject.CommonName, err)
			continue
		}

		logger.Printf("TLS: loaded certificate CN=%s from Local Machine MY store", cert.Subject.CommonName)

		tlsCert := tls.Certificate{
			Certificate: [][]byte{cert.Raw},
			PrivateKey:  key,
			Leaf:        cert,
		}

		return &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
			MinVersion:   tls.VersionTLS12,
		}, nil
	}

	if certSubject != "" {
		return nil, fmt.Errorf("no certificate matching %q found in Local Machine MY store", certSubject)
	}
	return nil, fmt.Errorf("no usable certificates found in Local Machine MY store")
}

// acquirePrivateKey calls CryptAcquireCertificatePrivateKey to get the CNG
// key handle and returns a crypto.Signer wrapping it.
func acquirePrivateKey(ctx *windows.CertContext, cert *x509.Certificate) (crypto.Signer, error) {
	var keyHandle uintptr
	var keySpec uint32
	var callerFreeInt int32

	r, _, err := procCryptAcquireCertPrivateKey.Call(
		uintptr(unsafe.Pointer(ctx)),
		uintptr(acquireOnlyNCryptKey|acquireSilentFlag),
		0,
		uintptr(unsafe.Pointer(&keyHandle)),
		uintptr(unsafe.Pointer(&keySpec)),
		uintptr(unsafe.Pointer(&callerFreeInt)),
	)
	if r == 0 {
		if err == syscall.Errno(0) {
			return nil, fmt.Errorf("CryptAcquireCertificatePrivateKey: no key")
		}
		return nil, fmt.Errorf("CryptAcquireCertificatePrivateKey: %w", err)
	}

	return &winKey{
		handle:     keyHandle,
		pub:        cert.PublicKey,
		callerFree: callerFreeInt != 0,
	}, nil
}
