package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type TreeEntry struct {
	Mode       string
	ObjectName string
	Hash       string
}

func decompressObject(hash string) ([]byte, error) {
	dir := hash[:2]
	file := hash[2:]

	blobPath := filepath.Join(".git/objects", dir, file)
	compressedBlob, err := os.ReadFile(blobPath)
	if err != nil {
		return nil, fmt.Errorf("error reading blob: %w", err)
	}

	// Create zlib reader for []byte
	reader := bytes.NewReader(compressedBlob)
	zlibReader, err := zlib.NewReader(reader)
	if err != nil {
		return nil, fmt.Errorf("error creating zlib reader: %w", err)
	}
	defer zlibReader.Close()

	// Read decompressed data
	blob, err := io.ReadAll(zlibReader)
	if err != nil {
		return nil, fmt.Errorf("error uncompressing blob: %w", err)
	}

	return blob, nil
}

func compressContent(content []byte) ([]byte, error) {
	// calculate SHA-1 of the decompressed content
	hashBytes := sha1.Sum(content)
	hashString := fmt.Sprintf("%x", hashBytes)

	// compress the content with zlib
	var buf bytes.Buffer
	writer := zlib.NewWriter(&buf)
	_, err := writer.Write(content)
	if err != nil {
		return nil, fmt.Errorf("error compressing content: %w", err)
	}

	err = writer.Close()
	if err != nil {
		return nil, fmt.Errorf("error closing zlib writer: %w", err)
	}

	// create the read-only blob file with the zlib compressed
	blobDir := filepath.Join(".git/objects", hashString[:2])
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		return nil, fmt.Errorf("error creating blob directory: %w", err)
	}

	blobFile := filepath.Join(blobDir, hashString[2:])
	compressedContent := buf.Bytes()
	if err := os.WriteFile(blobFile, compressedContent, 0444); err != nil {
		return nil, fmt.Errorf("error writing to blob file: %w", err)
	}

	return hashBytes[:], nil
}

func CreateCommitObject(treeSha string, parentSha string, message string) ([]byte, error) {
	tree := fmt.Sprintf("tree %s\n", treeSha)
	parent := fmt.Sprintf("parent %s\n", parentSha)

	user := "User <user@gmail.com>"
	now := time.Now()
	offset := now.Format("-0700")
	author := fmt.Sprintf("author %s %d %s\n", user, now.Unix(), offset)
	commiter := fmt.Sprintf("commiter %s %d %s\n", user, now.Unix(), offset)

	message = fmt.Sprintf("\n%s\n", message)

	body := tree + parent + author + commiter + message

	// create the commit object
	bodyBytes := []byte(body)
	header := fmt.Sprintf("commit %d\x00", len(bodyBytes))
	content := []byte(header)
	content = append(content, bodyBytes...)

	return compressContent(content)
}

func GetCommitTreeHash(commitObj []byte) (string, error) {
	// Skip header "commit <size>\0"
	nullPos := bytes.IndexByte(commitObj, '\x00')
	content := string(commitObj[nullPos+1:])

	// Find "tree <hash>" line
	lines := strings.SplitSeq(content, "\n")
	for line := range lines {
		if strings.HasPrefix(line, "tree ") {
			return strings.TrimSpace(line[5:]), nil
		}
	}

	return "", fmt.Errorf("no tree found in commit")
}

func CreateTreeObject(path string) ([]byte, error) {
	var objectBytes []byte

	// Read directory contents
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	// Sort entries by name
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".git") {
			continue
		}

		entryPath := filepath.Join(path, entry.Name())

		var hashBytes []byte
		if entry.IsDir() {
			// Recursively handle subdirectory
			hashBytes, err = CreateTreeObject(entryPath)
		} else {
			// Handle regular file
			hashBytes, err = CreateBlobObject(entryPath)
		}

		if err != nil {
			return nil, err
		}

		entryHeader := fmt.Sprintf("%s %s\x00", getFileMode(entry), entry.Name())
		objectBytes = append(objectBytes, []byte(entryHeader)...)
		objectBytes = append(objectBytes, hashBytes...)
	}

	header := fmt.Sprintf("tree %d\x00", len(objectBytes))
	content := []byte(header)
	content = append(content, objectBytes...)

	hash, err := compressContent(content)
	if err != nil {
		return nil, err
	}

	return []byte(hash), nil
}

func getFileMode(entry os.DirEntry) string {
	if entry.IsDir() {
		return "40000"
	}

	if entry.Type()&os.ModeSymlink != 0 {
		return "120000"
	}

	if fileInfo, err := entry.Info(); err == nil && fileInfo.Mode()&0111 != 0 {
		return "100755"
	}

	return "100644"
}

func parseTreeObject(treeSha string) ([]TreeEntry, error) {
	blob, err := decompressObject(treeSha)
	if err != nil {
		return nil, fmt.Errorf("failed to uncompress object %s: %v", treeSha, err)
	}

	nullPos := bytes.IndexByte(blob, '\x00')
	header := string(blob[:nullPos])
	treeContent := blob[nullPos+1:]

	if !strings.HasPrefix(header, "tree") {
		return nil, fmt.Errorf("invalid tree object: %s", treeSha)
	}

	var entries []TreeEntry
	i := 0
	for i < len(treeContent) {
		// find space for mode
		spacePos := bytes.IndexByte(treeContent[i:], ' ')
		mode := string(treeContent[i : i+spacePos])
		i += spacePos + 1

		// find null byte for object name
		nullPos := bytes.IndexByte(treeContent[i:], '\x00')
		objectName := string(treeContent[i : i+nullPos])
		i += nullPos + 1

		// 20-byte SHA
		sha := treeContent[i : i+20]
		i += 20

		treeEntry := TreeEntry{
			Mode:       mode,
			ObjectName: objectName,
			Hash:       fmt.Sprintf("%x", sha),
		}
		entries = append(entries, treeEntry)
	}

	return entries, nil
}

func GetTreeObject(treeSha string, isNameOnly bool) {
	entries, err := parseTreeObject(treeSha)
	if err != nil {
		handleErr("Failed to parse tree object %s: %v\n", treeSha, err)
	}

	for _, entry := range entries {
		if isNameOnly {
			fmt.Println(entry.ObjectName)
		} else {
			var objectType string

			switch entry.Mode {
			case "100644", "100755", "120000":
				objectType = "blob"
			case "040000":
				objectType = "tree"
			default:
				objectType = "commit"
			}

			fmt.Printf("%s %s %s %s\n", entry.Mode, objectType, entry.Hash, entry.ObjectName)
		}
	}
}

func CreateBlobObject(fileName string) ([]byte, error) {
	fileBytes, err := os.ReadFile(fileName)
	if err != nil {
		return nil, err
	}

	// create the blob content
	header := fmt.Sprintf("blob %d\x00", len(fileBytes))
	content := []byte(header)
	content = append(content, fileBytes...)

	return compressContent(content)
}

func GetBlobObject(hash string) (string, error) {
	blob, err := decompressObject(hash)
	if err != nil {
		return "", err
	}

	parts := strings.SplitN(string(blob), "\x00", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid blob")
	}

	return parts[1], nil
}

func RunGitInit() {
	for _, dir := range []string{".git", ".git/objects", ".git/refs"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			handleErr("Error creating directory: %s\n", err)
		}
	}

	headFileContents := []byte("ref: refs/heads/main\n")
	if err := os.WriteFile(".git/HEAD", headFileContents, 0644); err != nil {
		handleErr("Error writing file: %s\n", err)
	}
}

func handleErr(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(1)
}

// Usage: your_program.sh <command> <arg1> <arg2> ...
func main() {
	// You can use print statements as follows for debugging, they'll be visible when running tests.
	// fmt.Fprintf(os.Stderr, "Logs from your program will appear here!\n")

	if len(os.Args) < 2 {
		handleErr("usage: mygit <command> [<args>...]\n")
	}

	switch command := os.Args[1]; command {
	case "init":
		RunGitInit()
		fmt.Println("Initialized git directory")

	case "cat-file":
		if len(os.Args) < 4 || os.Args[2] != "-p" {
			handleErr("usage: mygit cat-file -p <sha1-hash>\n")
		}

		hash, err := GetBlobObject(os.Args[3])
		if err != nil {
			handleErr("Failed to read blob object: %v\n", err)
		}
		fmt.Print(hash)

	case "hash-object":
		if len(os.Args) < 4 || os.Args[2] != "-w" {
			handleErr("usage: mygit hash-object -w <sha1-hash>\n")
		}

		hashBytes, err := CreateBlobObject(os.Args[3])
		if err != nil {
			handleErr("Failed to create blob object: %v\n", err)
		}
		fmt.Printf("%x\n", hashBytes)

	case "ls-tree":
		if len(os.Args) < 3 {
			handleErr("usage: mygit ls-tree (--name-only) <tree-sha>\n")
		}

		if len(os.Args) == 3 {
			GetTreeObject(os.Args[2], false)
		} else {
			GetTreeObject(os.Args[3], os.Args[2] == "--name-only")
		}

	case "write-tree":
		dir, err := os.Getwd()
		if err != nil {
			handleErr("%s\n", err)
		}

		hashBytes, err := CreateTreeObject(dir)
		if err != nil {
			handleErr("Failed to create blob object: %v\n", err)
		}
		fmt.Printf("%x\n", hashBytes)

	case "commit-tree":
		if len(os.Args) < 7 {
			handleErr("usage: mygit commit-tree <tree_sha> -p <commit_sha> -m <message>\n")
		}

		hashBytes, err := CreateCommitObject(os.Args[2], os.Args[4], os.Args[6])
		if err != nil {
			handleErr("Failed to create blob object: %v\n", err)
		}
		fmt.Printf("%x\n", hashBytes)

	case "clone":
		if len(os.Args) < 4 {
			handleErr("usage: mygit clone https://github.com/blah1/blah2 <dest_dir>\n")
		}

		err := HandleClone(os.Args[2], os.Args[3])
		if err != nil {
			handleErr("Failed to clone repository: %v\n", err)
		}

	default:
		handleErr("Unknown command %s\n", command)
	}
}
