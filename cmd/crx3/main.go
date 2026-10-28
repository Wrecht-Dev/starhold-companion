// Command crx3 packs a browser extension directory into a CRX3 file and writes
// a Chrome update manifest (update.xml) for self-hosted force-install.
//
// Chrome/Chromium/Edge/Brave accept self-hosted CRX3 packages when the browser
// is told to force-install them via enterprise policy (ExtensionInstallForcelist
// or ExtensionSettings) pointing at an HTTPS update manifest. That is what makes
// a zero-click extension install possible without the Web Store.
//
// Usage:
//
//	crx3 -dir extension -key ext.pem -codebase https://host/companion/starhold-extension.crx \
//	     -crx out.crx -xml out/update.xml
//
// The signing key is created on first run (mode 0600) and must be kept: the
// extension ID is derived from it, and every native-messaging manifest pins it.
package main

import (
	"archive/zip"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	magic      = "Cr24"
	crxVersion = 3
	sigContext = "CRX3 SignedData\x00"
)

func main() {
	var (
		dir      = flag.String("dir", "extension", "extension directory to pack")
		keyPath  = flag.String("key", "extension-signing-key.pem", "RSA private key (PEM); created if missing")
		codebase = flag.String("codebase", "", "HTTPS URL the browser downloads the CRX from")
		crxOut   = flag.String("crx", "", "output CRX path (default <dir>.crx)")
		xmlOut   = flag.String("xml", "", "output update.xml path (default none)")
	)
	flag.Parse()

	key, err := loadOrCreateKey(*keyPath)
	must(err)
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	must(err)
	id := extensionID(sha256.Sum256(pubDER))
	fmt.Printf("extension id: %s\n", id)

	payload, err := zipDir(*dir)
	must(err)

	pubSum := sha256.Sum256(pubDER)
	crxID := make([]byte, 16)
	copy(crxID, pubSum[:16])
	signedHeader := protoBytes(1, crxID)
	digest := sha256.New()
	digest.Write([]byte(sigContext))
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(signedHeader)))
	digest.Write(n[:])
	digest.Write(signedHeader)
	digest.Write(payload)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest.Sum(nil))
	must(err)

	proof := append(protoBytes(1, pubDER), protoBytes(2, sig)...)
	header := append(protoBytes(2, rawMessage(proof)), protoBytes(10000, signedHeader)...)

	out := []byte(magic)
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], crxVersion)
	out = append(out, hdr[:]...)
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(header)))
	out = append(out, hdr[:]...)
	out = append(out, header...)
	out = append(out, payload...)

	crxPath := *crxOut
	if crxPath == "" {
		crxPath = strings.TrimSuffix(*dir, "/") + ".crx"
	}
	must(os.WriteFile(crxPath, out, 0o644))
	fmt.Printf("wrote %s (%d bytes)\n", crxPath, len(out))

	if *xmlOut != "" {
		if *codebase == "" {
			fatal("-codebase is required when writing update.xml")
		}
		ver := extensionVersion(*dir)
		xml := fmt.Sprintf(`<?xml version='1.0' encoding='UTF-8'?>
<gupdate xmlns='http://www.google.com/update2/response' protocol='2.0'>
  <app appid='%s'>
    <updatecheck codebase='%s' version='%s'/>
  </app>
</gupdate>
`, id, *codebase, ver)
		must(os.MkdirAll(filepath.Dir(*xmlOut), 0o755))
		must(os.WriteFile(*xmlOut, []byte(xml), 0o644))
		fmt.Printf("wrote %s (version %s)\n", *xmlOut, ver)
	}
}

// extensionID maps the first 16 bytes of the key digest to Chrome's a-p alphabet.
func extensionID(digest [32]byte) string {
	const alphabet = "abcdefghijklmnop"
	var b strings.Builder
	for _, c := range digest[:16] {
		b.WriteByte(alphabet[c>>4])
		b.WriteByte(alphabet[c&0x0f])
	}
	return b.String()
}

func extensionVersion(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return "0.0.0"
	}
	const marker = `"version"`
	i := strings.Index(string(data), marker)
	if i < 0 {
		return "0.0.0"
	}
	rest := string(data)[i+len(marker):]
	for _, quote := range []string{`"`, `'`} {
		start := strings.Index(rest, quote)
		if start < 0 {
			continue
		}
		rest = rest[start+1:]
		if end := strings.Index(rest, quote); end > 0 {
			return strings.TrimSpace(rest[:end])
		}
	}
	return "0.0.0"
}

// zipDir builds a deterministic zip (sorted names) of every file in dir.
func zipDir(dir string) ([]byte, error) {
	var files []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || strings.HasPrefix(filepath.Base(p), ".") {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)

	tmp, err := os.CreateTemp("", "crx3-*.zip")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	zw := zip.NewWriter(tmp)
	for _, f := range files {
		rel, err := filepath.Rel(dir, f)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(f)
		if err != nil {
			return nil, err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return nil, err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return nil, err
		}
		src, err := os.Open(f)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(w, src); err != nil {
			src.Close()
			return nil, err
		}
		src.Close()
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		return nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(tmp)
}

func loadOrCreateKey(path string) (*rsa.PrivateKey, error) {
	if data, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("%s: not a PEM file", path)
		}
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return key, nil
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	blob := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		return nil, err
	}
	fmt.Printf("created signing key %s — keep it: the extension id derives from it\n", path)
	return key, nil
}

// --- minimal protobuf encoding (wire format only, no codegen) --------------

// protoBytes encodes a length-delimited (wire type 2) field.
func protoBytes(field int, value []byte) []byte {
	out := protoVarint(uint64(field)<<3 | 2)
	out = append(out, protoVarint(uint64(len(value)))...)
	return append(out, value...)
}

func rawMessage(encoded []byte) []byte { return encoded }

func protoVarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

func must(err error) {
	if err != nil {
		fatal(err.Error())
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "crx3:", msg)
	os.Exit(1)
}
