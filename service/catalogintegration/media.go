package catalogintegration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	c "github.com/QuantumNous/new-api/pkg/catalogcontract"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type NativeFile struct {
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	MIME     string `json:"mime"`
	Filename string `json:"filename"`
	Data     []byte `json:"-"`
}
type nativeFilesKey struct{}

func MediaByteLimit() int64 {
	mb := constant.MaxFileDownloadMB
	if mb <= 0 {
		mb = 64
	}
	return int64(mb) * 1024 * 1024
}

// DecodeNativeInput reconstructs the same JSON descriptor used by the quote.
// Scalars are JSON literals on the integration wire, preserving zero/false.
func DecodeNativeInput(req *http.Request) (common.RawMessage, map[string]NativeFile, error) {
	contentType := req.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, nil, err
	}
	if mediaType != "multipart/form-data" {
		raw, err := io.ReadAll(io.LimitReader(req.Body, (2<<20)+1))
		if len(raw) > 2<<20 {
			return nil, nil, fmt.Errorf("native JSON exceeds 2 MiB")
		}
		return raw, nil, err
	}
	allowed := map[string]bool{}
	switch req.URL.Path {
	case "/v1/images/edits":
		allowed["image"] = true
		allowed["mask"] = true
	case "/v1/audio/transcriptions":
		allowed["file"] = true
	case "/v1/videos":
		allowed["input_reference"] = true
	default:
		return nil, nil, fmt.Errorf("multipart is not registered for this path")
	}
	reader := multipart.NewReader(io.LimitReader(req.Body, MediaByteLimit()+(2<<20)+1), params["boundary"])
	fields := map[string]common.RawMessage{}
	files := map[string]NativeFile{}
	var total, scalarTotal int64
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		name := part.FormName()
		if name == "" || fields[name] != nil {
			return nil, nil, fmt.Errorf("duplicate or unnamed multipart field")
		}
		limit := int64(2<<20) - scalarTotal
		if part.FileName() != "" {
			limit = MediaByteLimit() - total
		}
		data, err := io.ReadAll(io.LimitReader(part, limit+1))
		_ = part.Close()
		if err != nil || int64(len(data)) > limit {
			return nil, nil, fmt.Errorf("multipart part exceeds limit")
		}
		if part.FileName() == "" {
			var v common.RawMessage
			if common.Unmarshal(data, &v) != nil {
				return nil, nil, fmt.Errorf("multipart scalar must be a JSON literal")
			}
			scalarTotal += int64(len(data))
			fields[name] = v
			continue
		}
		_, disposition, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil {
			return nil, nil, err
		}
		filename := disposition["filename"]
		if !allowed[name] || filename == "" || filename == "." || filename == ".." || strings.ContainsAny(filename, "/\\\x00\r\n") || len(filename) > 255 {
			return nil, nil, fmt.Errorf("invalid native file field or filename")
		}
		contentType, _, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil {
			return nil, nil, err
		}
		if !strings.HasPrefix(contentType, "image/") && !strings.HasPrefix(contentType, "audio/") && !strings.HasPrefix(contentType, "video/") {
			return nil, nil, fmt.Errorf("unsupported media type")
		}
		if contentType == "image/svg+xml" {
			return nil, nil, fmt.Errorf("active image content is not supported")
		}
		hash := sha256.Sum256(data)
		file := NativeFile{SHA256: hex.EncodeToString(hash[:]), Size: int64(len(data)), MIME: contentType, Filename: filename, Data: data}
		files[name] = file
		fields[name], err = common.Marshal(file)
		if err != nil {
			return nil, nil, err
		}
		total += file.Size
	}
	raw, err := common.Marshal(fields)
	return raw, files, err
}

// NativeQuoteFiles validates immutable file metadata without fetching or
// manufacturing source media. Empty Data is used only by local quote parsers.
func NativeQuoteFiles(path string, raw common.RawMessage) (map[string]NativeFile, error) {
	names := []string{}
	switch path {
	case "/v1/audio/transcriptions":
		names = []string{"file"}
	case "/v1/images/edits":
		names = []string{"image", "mask"}
	case "/v1/videos":
		names = []string{"input_reference"}
	default:
		return nil, nil
	}
	var fields map[string]common.RawMessage
	if err := common.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	files := map[string]NativeFile{}
	var total int64
	for _, name := range names {
		value, exists := fields[name]
		if !exists {
			if name == "file" || name == "image" {
				return nil, fmt.Errorf("required native file descriptor missing")
			}
			continue
		}
		var file NativeFile
		if err := common.Unmarshal(value, &file); err != nil {
			return nil, err
		}
		hash, err := hex.DecodeString(file.SHA256)
		if err != nil || len(hash) != 32 || hex.EncodeToString(hash) != file.SHA256 || file.Size <= 0 || file.Size > MediaByteLimit()-total || file.Filename == "" || file.Filename == "." || file.Filename == ".." || len(file.Filename) > 255 || strings.ContainsAny(file.Filename, "/\\\x00\r\n") {
			return nil, fmt.Errorf("invalid native file descriptor")
		}
		mimeType, _, err := mime.ParseMediaType(file.MIME)
		if err != nil || mimeType != file.MIME || mimeType == "image/svg+xml" || (!strings.HasPrefix(mimeType, "image/") && !strings.HasPrefix(mimeType, "audio/") && !strings.HasPrefix(mimeType, "video/")) {
			return nil, fmt.Errorf("invalid native media type")
		}
		if (name == "image" || name == "mask" || name == "input_reference") && !strings.HasPrefix(mimeType, "image/") {
			return nil, fmt.Errorf("image field requires image media")
		}
		if name == "file" && !strings.HasPrefix(mimeType, "audio/") && !strings.HasPrefix(mimeType, "video/") {
			return nil, fmt.Errorf("transcription requires audio or video media")
		}
		total += file.Size
		files[name] = file
	}
	return files, nil
}

func ValidateNativeFiles(ctx context.Context, path string, raw common.RawMessage) error {
	descriptors, err := NativeQuoteFiles(path, raw)
	if err != nil {
		return err
	}
	files, _ := ctx.Value(nativeFilesKey{}).(map[string]NativeFile)
	if len(files) != len(descriptors) {
		return fmt.Errorf("native file bytes missing")
	}
	for name, expected := range descriptors {
		actual, ok := files[name]
		hash := sha256.Sum256(actual.Data)
		if !ok || hex.EncodeToString(hash[:]) != expected.SHA256 || int64(len(actual.Data)) != expected.Size || actual.MIME != expected.MIME || actual.Filename != expected.Filename {
			return fmt.Errorf("native file does not match quote")
		}
	}
	return nil
}

func WithNativeFiles(ctx context.Context, files map[string]NativeFile) context.Context {
	return context.WithValue(ctx, nativeFilesKey{}, files)
}

// NativeBody translates the verified integration descriptors back to the
// provider's ordinary multipart representation. Files never come from URLs.
func NativeBody(ctx context.Context, raw common.RawMessage) ([]byte, string, error) {
	files, _ := ctx.Value(nativeFilesKey{}).(map[string]NativeFile)
	if len(files) == 0 {
		return raw, "application/json", nil
	}
	var fields map[string]common.RawMessage
	if err := common.Unmarshal(raw, &fields); err != nil {
		return nil, "", err
	}
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for name, value := range fields {
		if file, ok := files[name]; ok {
			expected, err := common.Marshal(file)
			if err != nil {
				return nil, "", err
			}
			a, err := canonicalJSON(value)
			if err != nil {
				return nil, "", err
			}
			b, _ := canonicalJSON(expected)
			if !bytes.Equal(a, b) {
				return nil, "", fmt.Errorf("file descriptor mismatch")
			}
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": name, "filename": file.Filename}))
			header.Set("Content-Type", file.MIME)
			part, err := writer.CreatePart(header)
			if err != nil {
				return nil, "", err
			}
			if _, err = part.Write(file.Data); err != nil {
				return nil, "", err
			}
			continue
		}
		if err := writer.WriteField(name, common.JsonRawMessageToString(value)); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buffer.Bytes(), writer.FormDataContentType(), nil
}

// PersistArtifact uses content-addressed, server-owned paths, never filenames
// from a request. Its DB authorization record is written only after durable bytes.
func PersistArtifact(db *gorm.DB, root, principal, requestID, kind, mimeType string, data []byte) (c.Artifact, error) {
	if root == "" || !filepath.IsAbs(root) || len(data) == 0 || int64(len(data)) > MediaByteLimit() {
		return c.Artifact{}, fmt.Errorf("artifact store unavailable or content exceeds limit")
	}
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil || len(mediaType) > 128 {
		return c.Artifact{}, fmt.Errorf("invalid artifact content type")
	}
	h := sha256.Sum256(data)
	sum := hex.EncodeToString(h[:])
	id := model.CatalogKey(principal, requestID, sum, mediaType)
	if err = os.MkdirAll(root, 0700); err != nil {
		return c.Artifact{}, err
	}
	tmp, err := os.CreateTemp(root, ".catalog-")
	if err != nil {
		return c.Artifact{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err = tmp.Write(data); err != nil {
		return c.Artifact{}, err
	}
	if err = tmp.Sync(); err != nil {
		return c.Artifact{}, err
	}
	if err = tmp.Close(); err != nil {
		return c.Artifact{}, err
	}
	if err = os.Rename(tmp.Name(), filepath.Join(root, sum)); err != nil {
		return c.Artifact{}, err
	}
	directory, err := os.Open(root)
	if err != nil {
		return c.Artifact{}, err
	}
	err = directory.Sync()
	_ = directory.Close()
	if err != nil {
		return c.Artifact{}, err
	}
	row := model.CatalogArtifact{LookupKey: id, PrincipalID: principal, RequestID: requestID, ArtifactID: id, Checksum: sum, Size: int64(len(data)), MIME: mediaType}
	if err = db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return c.Artifact{}, err
	}
	return c.Artifact{ArtifactID: id, Type: kind, MIME: mediaType, Size: int64(len(data)), Checksum: sum, ReadPath: "/v1/invocations/" + requestID + "/artifacts/" + id}, nil
}

func (e *Engine) serveArtifact(ctx *gin.Context, p Principal, id, artifactID string) {
	var row model.CatalogArtifact
	err := e.DB.Where("lookup_key = ? AND principal_id = ? AND request_id = ?", artifactID, p.ID, id).Take(&row).Error
	if err != nil {
		WriteError(ctx, err)
		return
	}
	decodedHash, hashErr := hex.DecodeString(row.Checksum)
	if hashErr != nil || len(decodedHash) != 32 || !filepath.IsAbs(e.ArtifactRoot) {
		WriteError(ctx, fmt.Errorf("artifact unavailable"))
		return
	}
	file, err := os.Open(filepath.Join(e.ArtifactRoot, row.Checksum))
	if err != nil {
		WriteError(ctx, err)
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || stat.Size() != row.Size {
		WriteError(ctx, fmt.Errorf("artifact integrity mismatch"))
		return
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil || hex.EncodeToString(hash.Sum(nil)) != row.Checksum {
		WriteError(ctx, fmt.Errorf("artifact checksum mismatch"))
		return
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		WriteError(ctx, err)
		return
	}
	ctx.Header("Cache-Control", "private, no-store")
	ctx.Header("X-Content-Type-Options", "nosniff")
	ctx.Header("Content-Disposition", "attachment")
	ctx.Header("ETag", `"`+row.Checksum+`"`)
	ctx.DataFromReader(http.StatusOK, row.Size, row.MIME, file, nil)
	ctx.Abort()
}
