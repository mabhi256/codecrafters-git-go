package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Ref struct {
	Hash string
	Name string
}

type ObjectType int

const (
	OBJ_COMMIT    ObjectType = 1
	OBJ_TREE      ObjectType = 2
	OBJ_BLOB      ObjectType = 3
	OBJ_TAG       ObjectType = 4
	OBJ_OFS_DELTA ObjectType = 6
	OBJ_REF_DELTA ObjectType = 7
)

type DeltaObject struct {
	BaseHash  string
	DeltaData []byte
}

func parsePackFile(data []byte) error {
	// Skip 0008NAK\n
	packStart := bytes.Index(data, []byte("PACK"))
	if packStart == -1 {
		return fmt.Errorf("no PACK header found")
	}
	data = data[packStart:]

	// 4 bytes PACK + 4 bytes version + 4 byte number of objects
	numObjects := binary.BigEndian.Uint32(data[8:12])

	data = data[12:] // Position after header

	var deltaObjects []DeltaObject // Store deltas for second pass

	offset := 0
	for i := 0; i < int(numObjects); i++ {
		objType, _, objectOffset := parsePackHeader(data[offset:])
		offset += objectOffset
		compressedData := data[offset:]

		switch objType {
		case OBJ_COMMIT, OBJ_TREE, OBJ_BLOB, OBJ_TAG:
			// Regular objects - decompress directly
			objData, compressedOffset, err := decompressPackObject(compressedData)
			if err != nil {
				return fmt.Errorf("failed to decompress object %d: %w", i, err)
			}

			// Parse and store the object
			err = storePackObject(objType, objData)
			if err != nil {
				return fmt.Errorf("failed to store object %d: %w", i, err)
			}

			offset += compressedOffset

		case OBJ_OFS_DELTA:
			// We didn't set "ofs-delta" capability during negotiation, so we should only receive ref-delta
			return fmt.Errorf("OFS_DELTA objects not supported")

		case OBJ_REF_DELTA:
			// FIRST PASS: Just collect delta info, don't process yet
			// 20-byte object SHA-1
			baseObjectSHA := data[offset : offset+20]
			offset += 20

			// decompress delta pack object
			deltaData, compressedOffset, err := decompressPackObject(data[offset:])
			if err != nil {
				return fmt.Errorf("failed to decompress delta %d: %w", i, err)
			}

			// Store delta for second pass
			deltaObjects = append(deltaObjects, DeltaObject{
				BaseHash:  fmt.Sprintf("%x", baseObjectSHA),
				DeltaData: deltaData,
			})

			offset += compressedOffset

		default:
			return fmt.Errorf("unknown object type: %d", objType)
		}
	}

	// SECOND PASS: Resolve delta objects and delta-chains
	return processDeltaObjects(deltaObjects)
}

func parsePackHeader(data []byte) (ObjectType, uint64, int) {
	offset := 0

	// Object type (bits 6-4)
	objType := ObjectType((data[offset] >> 4) & 0x07) // 0b0000_0111

	// Initial size (bits 3-0)
	size := uint64(data[offset] & 0x0f) // 0b0000_1111
	shift := uint64(4)

	// If MSB is set, read more bytes for size
	for (data[offset] & 0x80) != 0 { // 0b1000_0000
		offset++
		size += uint64(data[offset]&0x7f) << shift // 0b0111_1111
		shift += 7
	}
	offset++ // move to body

	return objType, size, offset
}

func decompressPackObject(data []byte) ([]byte, int, error) {
	reader := bytes.NewReader(data)
	zlibReader, err := zlib.NewReader(reader)
	if err != nil {
		return nil, 0, fmt.Errorf("error creating zlib reader: %w", err)
	}
	defer zlibReader.Close()

	decompressed, err := io.ReadAll(zlibReader)
	if err != nil {
		return nil, 0, fmt.Errorf("error uncompressing blob: %w", err)
	}

	// Calculate how many compressed bytes we consumed
	bytesRemaining := reader.Len()
	compressedOffset := len(data) - bytesRemaining

	return decompressed, compressedOffset, nil
}

func storePackObject(objType ObjectType, rawContent []byte) error {
	// Convert ObjectType to string
	var typeStr string
	switch objType {
	case OBJ_COMMIT:
		typeStr = "commit"
	case OBJ_TREE:
		typeStr = "tree"
	case OBJ_BLOB:
		typeStr = "blob"
	case OBJ_TAG:
		typeStr = "tag"
	default:
		return fmt.Errorf("unknown object type: %d", objType)
	}

	// Create git object format: "type size\0content"
	header := fmt.Sprintf("%s %d\x00", typeStr, len(rawContent))
	gitObject := append([]byte(header), rawContent...)

	// Now store using your existing compressContent function
	_, err := compressContent(gitObject)
	return err
}

func processDeltaObjects(deltaObjects []DeltaObject) error {
	for _, obj := range deltaObjects {

		sourceLen, targetLen, instructionOffset := parseDeltaHeader(obj.DeltaData)

		baseContent, baseType, err := getBaseObject(obj.BaseHash)
		if err != nil {
			return fmt.Errorf("failed to parse base object: %w", err)
		}

		// Verify base object size matches delta header
		if uint64(len(baseContent)) != sourceLen {
			return fmt.Errorf("base object size mismatch: got %d, expected %d",
				len(baseContent), sourceLen)
		}

		// Parse instructions
		instructionData := obj.DeltaData[instructionOffset:]
		instructionOffset = 0 // reset instruction offset

		targetBuffer := make([]byte, targetLen)
		targetOffset := 0

		for instructionOffset < len(instructionData) {
			opcode := instructionData[instructionOffset]
			instructionOffset++

			if (opcode & 0x80) != 0 { // MSB is set, COPY instruction
				sourceOffset, copySize, packOffset, err := parseCopyInstruction(opcode, instructionData, instructionOffset)
				if err != nil {
					return fmt.Errorf("failed to parse copy instruction: %w", err)
				}

				// Copy the bytes from base content
				copy(targetBuffer[targetOffset:], baseContent[sourceOffset:sourceOffset+copySize])
				targetOffset += int(copySize)
				instructionOffset = packOffset
			} else {
				insertSize := int(opcode)

				copy(targetBuffer[targetOffset:], instructionData[instructionOffset:instructionOffset+insertSize])
				targetOffset += insertSize
				instructionOffset += insertSize
			}
		}

		storePackObject(baseType, targetBuffer)
	}

	return nil
}

func getBaseObject(baseHash string) ([]byte, ObjectType, error) {
	obj, err := decompressObject(baseHash)
	if err != nil {
		return nil, 0, err
	}

	// Parse the header: "type size\0content"
	nullPos := bytes.IndexByte(obj, '\x00')
	if nullPos == -1 {
		return nil, 0, fmt.Errorf("invalid object format")
	}

	header := string(obj[:nullPos])
	content := obj[nullPos+1:]

	// Extract object type
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return nil, 0, fmt.Errorf("invalid object header: %s", header)
	}

	var objType ObjectType
	switch parts[0] {
	case "commit":
		objType = OBJ_COMMIT
	case "tree":
		objType = OBJ_TREE
	case "blob":
		objType = OBJ_BLOB
	case "tag":
		objType = OBJ_TAG
	default:
		return nil, 0, fmt.Errorf("unknown object type: %s", parts[0])
	}

	return content, objType, nil
}

func parseDeltaHeader(data []byte) (sourceLen uint64, targetLen uint64, offset int) {
	offset = 0

	// Initial size (bits 6-0)
	sourceLen = uint64(data[offset] & 0x7f) // 0b0111_1111
	shift := uint64(7)

	// If MSB is set, read more bytes for size
	for (data[offset] & 0x80) != 0 { // 0b1000_0000
		offset++
		sourceLen += uint64(data[offset]&0x7f) << shift // 0b0111_1111
		shift += 7
	}
	offset++ // move to target section

	// Parse target length
	targetLen = uint64(data[offset] & 0x7f) // 0b0111_1111
	shift = 7

	// If MSB is set, read more bytes for size
	for (data[offset] & 0x80) != 0 { // 0b1000_0000
		offset++
		targetLen += uint64(data[offset]&0x7f) << shift // 0b0111_1111
		shift += 7
	}
	offset++ // move to instructions

	return sourceLen, targetLen, offset
}

func parseCopyInstruction(opcode byte, data []byte, offset int) (
	sourceOffset uint64, copySize uint64, newOffset int, err error,
) {
	// Parse offset using bits 3-0
	offsetBits := []byte{0x01, 0x02, 0x04, 0x08}
	sourceOffset = 0

	for i, bit := range offsetBits {
		if opcode&bit != 0 {
			sourceOffset |= uint64(data[offset]) << (i * 8)
			offset++
		}
	}

	// Parse size using bits 6-4
	sizeBits := []byte{0x10, 0x20, 0x40}
	copySize = 0

	for i, bit := range sizeBits {
		if opcode&bit != 0 {
			copySize |= uint64(data[offset]) << (i * 8)
			offset++
		}
	}

	// next instruction starts at offset+1
	return sourceOffset, copySize, offset, nil
}

func createRefs(ref *Ref) error {
	// Create refs/head directory
	dirPath := filepath.Join(".git", "refs", "head")
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		return fmt.Errorf("error creating refs/head directory: %w", err)
	}

	// Get branch name from default ref
	parts := strings.Split(ref.Name, "/")
	branch := parts[len(parts)-1]

	// Write commit hash to branch file
	refFilePath := filepath.Join(dirPath, branch)
	content := []byte(ref.Hash + "\n")
	if err := os.WriteFile(refFilePath, content, 0644); err != nil {
		return fmt.Errorf("error writing ref file: %w", err)
	}

	return nil
}

func checkoutWorkingDirectory(defaultRef *Ref) error {
	// Decompress default ref hash
	commitObj, err := decompressObject(defaultRef.Hash)
	if err != nil {
		return fmt.Errorf("error reading commit: %w", err)
	}

	// Parse commit object to get tree sha
	treeHash, err := GetCommitTreeHash(commitObj)
	if err != nil {
		return fmt.Errorf("error parsing commit: %w", err)
	}

	// Checkout the tree recursively
	return checkoutTree(treeHash, ".")
}

func checkoutTree(treeHash string, basePath string) error {
	entries, err := parseTreeObject(treeHash)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		fullPath := filepath.Join(basePath, entry.ObjectName)

		if entry.Mode == "40000" {
			// Create directory and recurse
			if err := os.MkdirAll(fullPath, 0755); err != nil {
				return fmt.Errorf("error creating directory %s: %w", fullPath, err)
			}
			if err := checkoutTree(entry.Hash, fullPath); err != nil {
				return err
			}
		} else {
			// Create file
			if err := checkoutFile(entry.Hash, fullPath, entry.Mode); err != nil {
				return err
			}
		}
	}

	return nil
}

func checkoutFile(blobHash string, filePath string, mode string) error {
	// Read blob content
	content, err := GetBlobObject(blobHash)
	if err != nil {
		return fmt.Errorf("error reading blob %s: %w", blobHash, err)
	}

	// Write file
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		return fmt.Errorf("error writing file %s: %w", filePath, err)
	}

	// Set executable permission if needed
	if mode == "100755" {
		if err := os.Chmod(filePath, 0755); err != nil {
			return fmt.Errorf("error setting executable permission: %w", err)
		}
	}

	return nil
}

// Just want the default branch pack files for this task
func requestPackFile(repoUrl string, defaultRef *Ref) ([]byte, error) {
	var requestBody bytes.Buffer

	// If we don't negotiate capabilities, we recieve raw packfiles with
	// no sideband multiplexing, no thin-pack or ofs-delta optimizations
	wantLine := fmt.Sprintf("want %s\n", defaultRef.Hash)
	totalLength := len(wantLine) + 4
	wantLine = fmt.Sprintf("%04x%s", totalLength, wantLine)
	requestBody.WriteString(wantLine)

	// Flush packet
	requestBody.WriteString("0000")

	// Done Line
	doneLine := "done\n"
	doneLength := len(doneLine) + 4
	doneLine = fmt.Sprintf("%04x%s", doneLength, doneLine)
	requestBody.WriteString(doneLine)

	url := fmt.Sprintf("%s/git-upload-pack", repoUrl)
	resp, err := http.Post(url, "application/x-git-upload-pack-request", &requestBody)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

func parsePacketLines(data []byte) (*Ref, []Ref, []string, error) {
	var defaultRef Ref
	var refs []Ref
	var capabilities []string

	offset := 0
	isServiceAnnounced := false
	isCapabilitiesParsed := false

	for offset < len(data) {
		if offset+4 > len(data) {
			break
		}

		lengthHex := string(data[offset : offset+4])
		length, err := strconv.ParseInt(lengthHex, 16, 32)
		if err != nil {
			return nil, nil, nil, err
		}

		if length == 0 {
			offset += 4 // flush packet - skip
			continue
		}

		// The first four bytes represent the length of the entire string
		// (including the leading 4 length bytes)
		lineStart := offset + 4
		lineEnd := offset + int(length)
		line := data[lineStart:lineEnd]

		// Skip services announcement
		if !isServiceAnnounced && bytes.HasPrefix(line, []byte("# service=git-upload-pack")) {
			isServiceAnnounced = true
			offset += int(length)
			continue
		}

		// Extract hash (first 40 bytes)
		hash := string(line[:40])

		// Remove trailing newline if present
		if len(line) > 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}

		if !isCapabilitiesParsed {
			if nullIdx := bytes.IndexByte(line, '\x00'); nullIdx != -1 {
				refName := string(line[41:nullIdx])
				refs = append(refs, Ref{Hash: hash, Name: refName})

				capStr := string(line[nullIdx+1:])

				if symrefIdx := strings.Index(capStr, "symref"); symrefIdx != -1 {
					defaultBranchStart := symrefIdx + 12 // "symref=HEAD:"
					defaultBranchEnd := defaultBranchStart + strings.Index(capStr[defaultBranchStart:], " ")
					defaultBranch := capStr[defaultBranchStart:defaultBranchEnd]
					defaultRef = Ref{Hash: hash, Name: defaultBranch}

					capStr = capStr[:symrefIdx]
					capabilities = strings.Fields(capStr)
					isCapabilitiesParsed = true
					// ---- we can stop here since we only need default ref for checkout
					break
				}
			}
		} else {
			// refName := string(line[41:])
			// refs = append(refs, Ref{Hash: hash, Name: refName})
		}

		offset += int(length)
	}

	return &defaultRef, refs, capabilities, nil
}

func discoverDefaultRef(repoUrl string) (*Ref, error) {
	url := fmt.Sprintf("%s/info/refs?service=git-upload-pack", repoUrl)
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch git-upload-pack: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("unable to read git-upload-pack response body: %w", err)
	}

	defaultRef, _, _, err := parsePacketLines(body)
	if err != nil {
		return nil, fmt.Errorf("unable to parse git-upload-pack lines: %w", err)
	}

	return defaultRef, nil
}

// Assuming repo is https://github.com/blah1/blah2
func HandleClone(repoUrl string, dest string) error {
	// 1. Create dest dir
	if err := os.MkdirAll(dest, 0755); err != nil {
		return fmt.Errorf("unable to create dest directory: %w", err)
	}

	// 2. cd to dest
	err := os.Chdir(dest)
	if err != nil {
		return fmt.Errorf("unable to cd to dest directory: %w", err)
	}

	// 3. Run 'git init' in dest directory
	RunGitInit()

	// 4. Reference Discovery
	// This is equivalent to running 'git remote add origin <repo_url>' and 'git ls-remote origin'
	defaultRef, err := discoverDefaultRef(repoUrl)
	if err != nil {
		return err
	}

	// 5. Request default branch pack file (git fetch-pack origin <commit_hash>)
	// This commit_hash is the HEAD commit of the default branch
	// Client's git fetch-pack triggers git upload-pack in the git-server
	packfileData, err := requestPackFile(repoUrl, defaultRef)
	if err != nil {
		return fmt.Errorf("unable to request packfile: %w", err)
	}

	// 6. Parse packfiles (git unpack-objects < packfile)
	// Git actually runs 'git index-pack .git/objects/pack/tmp_pack_XXXXXX'
	// which creates an index file (.git/index)
	err = parsePackFile(packfileData)
	if err != nil {
		return fmt.Errorf("unable to parse packfile: %w", err)
	}

	// 7. Create refs (git update-ref <default_branch> <commit_hash>)
	// Step 4, 5, 6, 7 are part of 'git fetch origin'
	err = createRefs(defaultRef)
	if err != nil {
		return fmt.Errorf("unable to create refs: %w", err)
	}

	// 8. Update HEAD (git symbolic-ref HEAD <default_branch>)
	// We hardcode HEAD in git init but the default branch may be different
	content := fmt.Sprintf("ref: %s\n", defaultRef.Name)
	contentBytes := []byte(content)
	if err := os.WriteFile(".git/HEAD", contentBytes, 0644); err != nil {
		handleErr("Error writing HEAD file: %s\n", err)
	}

	// 9. Checkout working directory (git checkout <commit_hash>)
	// Git actually runs 'git read-tree <tree_hash>' and then 'git checkout-index --all'
	// Git also sets up upstream remote for fetch and push (git config branch.<branch_name>.remote origin)
	// and upstream merge branch to merge with during a git pull (git config branch.<branch_name>.merge refs/heads/<branch_name>)
	// But we don't need to implement this.
	err = checkoutWorkingDirectory(defaultRef)
	if err != nil {
		return fmt.Errorf("unable to checkout working directory: %w", err)
	}

	return nil
}
