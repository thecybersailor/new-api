package catalogintegration

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	c "github.com/QuantumNous/new-api/pkg/catalogcontract"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// NativeStream forwards provisional SSE while the adapter also retains bounded
// bytes for recovery. A disconnected reader never cancels or replays provider I/O.
type NativeStream struct {
	mu                    sync.Mutex
	writer                http.ResponseWriter
	started, disconnected bool
}
type nativeStreamKey struct{}

func NativeStreamFromContext(ctx context.Context) *NativeStream {
	stream, _ := ctx.Value(nativeStreamKey{}).(*NativeStream)
	return stream
}
func (s *NativeStream) Write(header http.Header, status int, data []byte) {
	if s == nil || !strings.Contains(header.Get("Content-Type"), "text/event-stream") {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disconnected {
		return
	}
	if !s.started {
		s.started = true
		s.writer.Header().Set("Content-Type", header.Get("Content-Type"))
		s.writer.Header().Set("Cache-Control", "no-store")
		s.writer.WriteHeader(status)
	}
	if _, err := s.writer.Write(data); err != nil {
		s.disconnected = true
		return
	}
	if flusher, ok := s.writer.(http.Flusher); ok {
		flusher.Flush()
	}
}
func (s *NativeStream) Started() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.started }

func WriteError(ctx *gin.Context, err error) {
	status := http.StatusInternalServerError
	out := &c.Error{Code: "INTEGRATION_UNAVAILABLE", Message: "integration service is unavailable", NextAction: "retry observation; do not replay a submission", Retryable: true}
	var known *c.Error
	if errors.As(err, &known) {
		out = known
		status = http.StatusBadRequest
		switch out.Code {
		case "UNAUTHORIZED":
			status = http.StatusUnauthorized
		case "ACCESS_DENIED":
			status = http.StatusForbidden
		case "IDEMPOTENCY_CONFLICT", "INVOCATION_CONFLICT", "SCOPE_CHANGED", "RECEIPT_MISMATCH":
			status = http.StatusConflict
		case "SUBMISSION_UNKNOWN":
			status = http.StatusAccepted
		case "OPERATION_NOT_ADMITTED", "QUOTE_BOUND_UNAVAILABLE":
			status = http.StatusUnprocessableEntity
		case "INTEGRATION_DISABLED":
			status = http.StatusServiceUnavailable
		}
	} else if errors.Is(err, gorm.ErrRecordNotFound) {
		status = http.StatusNotFound
		out = &c.Error{Code: "NOT_FOUND", Message: "integration resource not found", NextAction: "verify the authorized resource identifier"}
	}
	ctx.Header("Cache-Control", "no-store")
	ctx.AbortWithStatusJSON(status, out)
}

// Handle owns only managed integration requests. Legacy routes are left to the
// existing router; native paths remain native and never gain a generic submit API.
func (e *Engine) Handle(ctx *gin.Context, p Principal) {
	ctx.Header("Cache-Control", "no-store")
	path := ctx.Request.URL.Path
	if ctx.Request.Method == http.MethodGet && path == "/v1/catalog" {
		if ctx.Query("cursor") != "" {
			WriteError(ctx, c.Invalid("CATALOG_SNAPSHOT_INCOMPLETE", "this snapshot is returned as one complete page", "request without a cursor"))
			return
		}
		out, err := e.Catalog(ctx.Request.Context(), p, ctx.Query("snapshotId"))
		if err != nil {
			WriteError(ctx, err)
			return
		}
		ctx.AbortWithStatusJSON(http.StatusOK, out)
		return
	}
	if ctx.Request.Method == http.MethodGet && strings.HasPrefix(path, "/v1/invocations/") {
		id := strings.TrimPrefix(path, "/v1/invocations/")
		if requestID, artifactID, ok := strings.Cut(id, "/artifacts/"); ok {
			if !ValidRequestID(requestID) || !ValidRequestID(artifactID) {
				WriteError(ctx, c.Invalid("REQUEST_INVALID", "invalid artifact identity", "use the invocation artifact readPath"))
				return
			}
			e.serveArtifact(ctx, p, requestID, artifactID)
			return
		}
		if !ValidRequestID(id) {
			WriteError(ctx, c.Invalid("REQUEST_INVALID", "invalid invocation ID", "use the original clientRequestId"))
			return
		}
		out, err := e.Invocation(ctx.Request.Context(), p, id, true)
		if err != nil {
			WriteError(ctx, err)
			return
		}
		ctx.AbortWithStatusJSON(http.StatusOK, out)
		return
	}
	if ctx.Request.Method != http.MethodPost {
		WriteError(ctx, c.Invalid("OPERATION_NOT_ADMITTED", "integration route is not supported", "use the catalog, quotes or invocation API"))
		return
	}
	raw, files, err := DecodeNativeInput(ctx.Request)
	if err != nil {
		WriteError(ctx, c.Invalid("REQUEST_INVALID", err.Error(), "send the declared native input"))
		return
	}
	requestContext := WithNativeFiles(ctx.Request.Context(), files)
	if path == "/v1/quotes" {
		var r c.QuoteRequest
		if common.Unmarshal(raw, &r) != nil {
			WriteError(ctx, c.Invalid("REQUEST_INVALID", "invalid quote JSON", "supply the quote contract"))
			return
		}
		out, err := e.Quote(ctx.Request.Context(), p, r)
		if err != nil {
			WriteError(ctx, err)
			return
		}
		ctx.AbortWithStatusJSON(http.StatusOK, out)
		return
	}
	if err = c.ValidateNativePath(path); err != nil {
		WriteError(ctx, err)
		return
	}
	stream := &NativeStream{writer: ctx.Writer}
	requestContext = context.WithValue(requestContext, nativeStreamKey{}, stream)
	out, err := e.Submit(requestContext, p, path, ctx.GetHeader("X-DanToken-Quote-ID"), ctx.GetHeader("Idempotency-Key"), raw)
	if stream.Started() {
		ctx.Abort()
		return
	} // Invocation remains the authoritative terminal record.
	if err != nil {
		WriteError(ctx, err)
		return
	}
	// A recovered native response can be returned verbatim; unfinished operations
	// return a 202 invocation that the client observes without another submission.
	if out.ExecutionState == "succeeded" {
		invocation, err := model.FindCatalogInvocation(e.DB, p.ID, out.ClientRequestID)
		if err != nil {
			WriteError(ctx, err)
			return
		}
		var execution model.CatalogExecution
		err = e.DB.Where("quote_id = ?", invocation.QuoteID).Take(&execution).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			WriteError(ctx, err)
			return
		}
		if err == nil && len(execution.NativeBody) > 0 {
			ctx.Header("X-Content-Type-Options", "nosniff")
			ctx.Data(http.StatusOK, execution.ContentType, execution.NativeBody)
			ctx.Abort()
			return
		}
	}
	if len(out.Result) > 0 && out.ExecutionState == "succeeded" {
		ctx.Data(http.StatusOK, "application/json", out.Result)
		ctx.Abort()
		return
	}
	ctx.AbortWithStatusJSON(http.StatusAccepted, out)
}
