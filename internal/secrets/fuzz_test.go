package secrets

import (
	"bytes"
	"testing"
	"unicode/utf8"
)

func FuzzEncryptedConnection(f *testing.F) {
	f.Add("https://arr.example", "example-key", "alice", "example-password", uint64(0), byte(0))
	f.Add("", "", "", "", uint64(31), byte(255))
	f.Fuzz(func(t *testing.T, endpoint, apiKey, username, password string, index uint64, bit byte) {
		for _, s := range []string{endpoint, apiKey, username, password} {
			if !utf8.ValidString(s) || len(s) > 16384 {
				return // JSON replaces invalid UTF-8; it cannot round-trip those strings.
			}
		}
		key := bytes.Repeat([]byte{7}, 32)
		conn := Connection{URL: endpoint, APIKey: apiKey, Username: username, Password: password, Enabled: true, ExternalURL: endpoint}
		blob, err := encrypt(key, conn)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decrypt(key, blob)
		if err != nil || got != conn {
			t.Fatalf("encrypted connection did not round-trip: %v", err)
		}
		// Every bit of both the nonce and ciphertext must be authenticated.
		mutated := encryptedBlob{Nonce: bytes.Clone(blob.Nonce), Ciphertext: bytes.Clone(blob.Ciphertext)}
		i := index % uint64(len(mutated.Nonce)+len(mutated.Ciphertext))
		if i < uint64(len(mutated.Nonce)) {
			mutated.Nonce[i] ^= 1 << (bit % 8)
		} else {
			mutated.Ciphertext[i-uint64(len(mutated.Nonce))] ^= 1 << (bit % 8)
		}
		if _, err := decrypt(key, mutated); err == nil {
			t.Fatal("accepted a tampered encrypted connection")
		}
		key[index%uint64(len(key))] ^= 1 << (bit % 8)
		if _, err := decrypt(key, blob); err == nil {
			t.Fatal("accepted a different encryption key")
		}
	})
}

func FuzzDecryptMalformed(f *testing.F) {
	key := bytes.Repeat([]byte{7}, 32)
	blob, err := encrypt(key, Connection{URL: "https://arr.example"})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(blob.Nonce, blob.Ciphertext)
	f.Add([]byte{}, []byte{})
	f.Add([]byte{1}, []byte{2})
	f.Fuzz(func(t *testing.T, nonce, ciphertext []byte) {
		// Hand-edited or corrupt stores must produce an error, never a panic.
		conn, err := decrypt(key, encryptedBlob{Nonce: nonce, Ciphertext: ciphertext})
		if err != nil {
			return
		}
		roundTrip, err := encrypt(key, conn)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decrypt(key, roundTrip)
		if err != nil || got != conn {
			t.Fatalf("accepted connection cannot be saved and read: %v", err)
		}
	})
}
