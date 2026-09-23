// Command cryptocheck encrypts or decrypts a value with GoClaw's AES-GCM
// helper. The upgrade rehearsal uses it to prove that API keys written by an
// old install stay readable after migrations (encryption-key continuity).
//
//	cryptocheck encrypt <plaintext>   # key from GOCLAW_ENCRYPTION_KEY
//	cryptocheck decrypt <ciphertext>
package main

import (
	"fmt"
	"os"

	"github.com/nextlevelbuilder/goclaw/internal/crypto"
)

func main() {
	key := os.Getenv("GOCLAW_ENCRYPTION_KEY")
	if key == "" || len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: GOCLAW_ENCRYPTION_KEY=... cryptocheck encrypt|decrypt <value>")
		os.Exit(2)
	}
	var (
		out string
		err error
	)
	switch os.Args[1] {
	case "encrypt":
		out, err = crypto.Encrypt(os.Args[2], key)
	case "decrypt":
		if !crypto.IsEncrypted(os.Args[2]) {
			fmt.Fprintln(os.Stderr, "value is not encrypted")
			os.Exit(1)
		}
		out, err = crypto.Decrypt(os.Args[2], key)
	default:
		err = fmt.Errorf("unknown op %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(out)
}
