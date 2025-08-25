package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func RunHashObject(fileName string) {
	fileBytes, err := os.ReadFile(fileName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading file: %s\n", err)
	}

	// create the blob content
	header := fmt.Sprintf("blob %d\x00", len(fileBytes))
	content := []byte(header)
	content = append(content, fileBytes...)

	// calculate SHA-1 of the uncompressed content
	hashBytes := sha1.Sum(content)
	hashString := fmt.Sprintf("%x", hashBytes)

	// compress the content with zlib
	var buf bytes.Buffer
	writer := zlib.NewWriter(&buf)
	_, err = writer.Write(content)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error compressing content: %s\n", err)
	}

	err = writer.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error closing zlib writer: %s\n", err)
	}

	// create the read-only blob file with the zlib compressed
	blobDir := filepath.Join(".git/objects", hashString[:2])
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating blob directory: %s\n", err)
	}

	blobFile := filepath.Join(blobDir, hashString[2:])
	compressedContent := buf.Bytes()
	if err := os.WriteFile(blobFile, compressedContent, 0444); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing to blob file: %s\n", err)
	}

	fmt.Printf("%x", hashBytes)
}

func RunCatFile(hash string) {
	dir := hash[:2]
	file := hash[2:]

	blobPath := filepath.Join(".git/objects", dir, file)
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

	parts := strings.SplitN(string(blob), "\x00", 2)
	if len(parts) != 2 {
		fmt.Fprintf(os.Stderr, "Invalid blob\n")
		return
	}
	fmt.Print(parts[1])
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

	case "hash-object":
		if len(os.Args) < 4 || os.Args[2] != "-w" {
			fmt.Fprintf(os.Stderr, "usage: mygit hash-object -w <sha1-hash>\n")
			os.Exit(1)
		}
		RunHashObject(os.Args[3])

	default:
		fmt.Fprintf(os.Stderr, "Unknown command %s\n", command)
		os.Exit(1)
	}
}
