package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestCRX3RoundTrip packs a directory, then parses the produced CRX3 back:
// header magic/version, protobuf fields, signature over the documented
// "CRX3 SignedData" context, and the embedded zip payload. This is the evidence
// that a Chrome-family force-install will accept the artifact.
func TestCRX3RoundTrip(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	payload := mustZip(t)
	crx := buildCRX(t, key, pubDER, payload)

	if string(crx[:4]) != magic {
		t.Fatalf("magic = %q", crx[:4])
	}
	if v := binary.LittleEndian.Uint32(crx[4:8]); v != crxVersion {
		t.Fatalf("version = %d", v)
	}
	headerLen := int(binary.LittleEndian.Uint32(crx[8:12]))
	header := crx[12 : 12+headerLen]
	if !bytes.Equal(crx[12+headerLen:], payload) {
		t.Fatal("payload mismatch")
	}

	proof, signedHeader := parseHeader(t, header)
	if !bytes.Equal(proof.pub, pubDER) {
		t.Fatal("public key in header does not match")
	}
	if len(signedHeader) == 0 {
		t.Fatal("missing signed_header_data")
	}

	digest := sha256.New()
	digest.Write([]byte(sigContext))
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(signedHeader)))
	digest.Write(n[:])
	digest.Write(signedHeader)
	digest.Write(payload)
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest.Sum(nil), proof.sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}

	// crx_id inside signed_header_data must be the first 16 bytes of SHA-256 of
	// the public key: Chrome derives the extension ID from exactly that.
	sum := sha256.Sum256(pubDER)
	if !bytes.Equal(proof.crxID, sum[:16]) {
		t.Fatalf("crx_id mismatch: %x vs %x", proof.crxID, sum[:16])
	}
	if id := extensionID(sum); len(id) != 32 {
		t.Fatalf("extension id %q is not 32 chars", id)
	}
}

func TestZipDirSkipsDotfiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.js"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := zipDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("a.js")) || bytes.Contains(data, []byte(".hidden")) {
		t.Fatal("zip entries are wrong")
	}
}

type proofData struct {
	pub   []byte
	sig   []byte
	crxID []byte
}

// parseHeader walks the protobuf wire format by hand: field 2 (sha256_with_rsa
// message with public_key/signature) and field 10000 (signed_header_data).
func parseHeader(t *testing.T, b []byte) (proofData, []byte) {
	t.Helper()
	var out proofData
	var signed []byte
	for len(b) > 0 {
		field, rest := readVarint(t, b)
		num := int(field >> 3)
		if field&7 != 2 {
			t.Fatalf("unexpected wire type %d", field&7)
		}
		l, rest := readVarint(t, rest)
		val := rest[:l]
		b = rest[l:]
		switch num {
		case 2:
			for len(val) > 0 {
				f2, r2 := readVarint(t, val)
				ln, r3 := readVarint(t, r2)
				v := r3[:ln]
				val = r3[ln:]
				switch f2 >> 3 {
				case 1:
					out.pub = v
				case 2:
					out.sig = v
				}
			}
		case 10000:
			signed = val
			for len(val) > 0 {
				f3, r3 := readVarint(t, val)
				ln, r4 := readVarint(t, r3)
				v := r4[:ln]
				val = r4[ln:]
				if f3>>3 == 1 {
					out.crxID = v
				}
			}
		}
	}
	return out, signed
}

func readVarint(t *testing.T, b []byte) (uint64, []byte) {
	t.Helper()
	var v uint64
	var shift uint
	for i := 0; i < len(b); i++ {
		v |= uint64(b[i]&0x7f) << shift
		if b[i]&0x80 == 0 {
			return v, b[i+1:]
		}
		shift += 7
	}
	t.Fatal("truncated varint")
	return 0, nil
}

func buildCRX(t *testing.T, key *rsa.PrivateKey, pubDER, payload []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(pubDER)
	crxID := make([]byte, 16)
	copy(crxID, sum[:16])
	signedHeader := protoBytes(1, crxID)
	digest := sha256.New()
	digest.Write([]byte(sigContext))
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(signedHeader)))
	digest.Write(n[:])
	digest.Write(signedHeader)
	digest.Write(payload)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	proof := append(protoBytes(1, pubDER), protoBytes(2, sig)...)
	header := append(protoBytes(2, proof), protoBytes(10000, signedHeader)...)
	out := []byte(magic)
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], crxVersion)
	out = append(out, hdr[:]...)
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(header)))
	out = append(out, hdr[:]...)
	out = append(out, header...)
	return append(out, payload...)
}

func mustZip(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":"0.2.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := zipDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
