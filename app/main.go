package main

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func RunCatFile(hash string) {
	dir := hash[:2]
	file := hash[2:]

	blobPath := filepath.Join(".git/objects/", dir, file)
	compressedBlob, err := os.ReadFile(blobPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading blob: %s\n", err)
	}

	// Create zlib reader for []byte
	reader := bytes.NewReader(compressedBlob)
	zlibReader, err := zlib.NewReader(reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating zlib reader: %s\n", err)
		return
	}
	defer zlibReader.Close()

	// Read uncompressed data
	blob, err := io.ReadAll(zlibReader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error uncompressing blob: %s\n", err)
		return
	}

	x := strings.Split(string(blob), "\x00")
	fmt.Print(x[1])
}

func RunInit() {
	for _, dir := range []string{".git", ".git/objects", ".git/refs"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating directory: %s\n", err)
		}
	}

	headFileContents := []byte("ref: refs/heads/main\n")
	if err := os.WriteFile(".git/HEAD", headFileContents, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing file: %s\n", err)
	}

	fmt.Println("Initialized git directory")
}

// Usage: your_program.sh <command> <arg1> <arg2> ...
func main() {
	// You can use print statements as follows for debugging, they'll be visible when running tests.
	// fmt.Fprintf(os.Stderr, "Logs from your program will appear here!\n")

	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: mygit <command> [<args>...]\n")
		os.Exit(1)
	}

	switch command := os.Args[1]; command {
	case "init":
		RunInit()

	case "cat-file":
		if len(os.Args) < 4 || os.Args[2] != "-p" {
			fmt.Fprintf(os.Stderr, "usage: mygit cat-file -p <sha1-hash>\n")
			os.Exit(1)
		}
		RunCatFile(os.Args[3])

	default:
		fmt.Fprintf(os.Stderr, "Unknown command %s\n", command)
		os.Exit(1)
	}
}
