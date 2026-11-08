package controller

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	c "github.com/QuantumNous/new-api/pkg/catalogcontract"
	"github.com/QuantumNous/new-api/service"
	integration "github.com/QuantumNous/new-api/service/catalogintegration"
)

// aggregateCatalogSSE retains native bytes separately and builds a recoverable
// JSON result. It requires a protocol terminal event, never treating EOF as done.
func aggregateCatalogSSE(path string, raw []byte) (common.RawMessage, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), int(integration.MediaByteLimit()))
	data := []string{}
	done := false
	var final common.RawMessage
	choices := map[int]map[string]any{}
	tools := map[int]map[int]map[string]any{}
	response := map[string]any{"object": "chat.completion"}
	images := []any{}
	consume := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		if payload == "[DONE]" {
			done = true
			return nil
		}
		var event map[string]any
		if err := common.Unmarshal([]byte(payload), &event); err != nil {
			return err
		}
		if typ, _ := event["type"].(string); typ == "response.completed" {
			final, _ = common.Marshal(event["response"])
			done = true
			return nil
		} else if typ == "response.failed" || typ == "error" {
			return fmt.Errorf("provider stream failed")
		}
		if typ, _ := event["type"].(string); typ == "image_generation.completed" || typ == "image_edit.completed" {
			images = append(images, event)
			return nil
		}
		for _, key := range []string{"id", "model", "created", "usage", "system_fingerprint"} {
			if value, ok := event[key]; ok && value != nil {
				response[key] = value
			}
		}
		chunks, _ := event["choices"].([]any)
		for _, value := range chunks {
			chunk, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid chat stream choice")
			}
			indexValue, ok := chunk["index"].(float64)
			if !ok || indexValue < 0 || indexValue > 128 || indexValue != float64(int(indexValue)) {
				return fmt.Errorf("invalid choice index")
			}
			index := int(indexValue)
			choice := choices[index]
			if choice == nil {
				choice = map[string]any{"index": index, "message": map[string]any{"role": "assistant", "content": ""}}
				choices[index] = choice
			}
			message := choice["message"].(map[string]any)
			if reason, ok := chunk["finish_reason"].(string); ok && reason != "" {
				choice["finish_reason"] = reason
			}
			delta, _ := chunk["delta"].(map[string]any)
			for _, key := range []string{"content", "reasoning_content", "reasoning"} {
				if fragment, ok := delta[key].(string); ok {
					old, _ := message[key].(string)
					message[key] = old + fragment
				}
			}
			if role, ok := delta["role"].(string); ok {
				message["role"] = role
			}
			toolChunks, _ := delta["tool_calls"].([]any)
			for _, value := range toolChunks {
				chunk, ok := value.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid tool delta")
				}
				i, ok := chunk["index"].(float64)
				if !ok || i < 0 || i > 128 || i != float64(int(i)) {
					return fmt.Errorf("invalid tool index")
				}
				if tools[index] == nil {
					tools[index] = map[int]map[string]any{}
				}
				tool := tools[index][int(i)]
				if tool == nil {
					tool = map[string]any{"type": "function", "function": map[string]any{"name": "", "arguments": ""}}
					tools[index][int(i)] = tool
				}
				if id, ok := chunk["id"].(string); ok {
					tool["id"] = id
				}
				function, _ := chunk["function"].(map[string]any)
				for _, key := range []string{"name", "arguments"} {
					if fragment, ok := function[key].(string); ok {
						f := tool["function"].(map[string]any)
						old, _ := f[key].(string)
						f[key] = old + fragment
					}
				}
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := consume(); err != nil {
				return nil, err
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(data) > 0 {
		return nil, fmt.Errorf("incomplete SSE frame")
	}
	if !done {
		return nil, fmt.Errorf("stream has no terminal marker")
	}
	if path == "/v1/responses" {
		if len(final) == 0 {
			return nil, fmt.Errorf("missing terminal Response")
		}
		return final, nil
	}
	if strings.HasPrefix(path, "/v1/images/") {
		return common.Marshal(map[string]any{"data": images})
	}
	ordered := []any{}
	for i := 0; i < len(choices); i++ {
		choice := choices[i]
		if choice == nil {
			return nil, fmt.Errorf("noncontiguous chat choices")
		}
		if reason, ok := choice["finish_reason"].(string); !ok || reason == "" {
			return nil, fmt.Errorf("chat choice has no terminal finish reason")
		}
		if len(tools[i]) > 0 {
			calls := []any{}
			for j := 0; j < len(tools[i]); j++ {
				if tools[i][j] == nil {
					return nil, fmt.Errorf("noncontiguous tool deltas")
				}
				calls = append(calls, tools[i][j])
			}
			choice["message"].(map[string]any)["tool_calls"] = calls
		}
		ordered = append(ordered, choice)
	}
	if len(ordered) == 0 {
		return nil, fmt.Errorf("empty chat stream")
	}
	response["choices"] = ordered
	return common.Marshal(response)
}

func (a *catalogRelayAdapter) archiveNativeMedia(ctx context.Context, row model.CatalogStoredQuote, execution model.CatalogExecution, out *integration.Outcome) error {
	if len(out.Artifacts) > 0 {
		return nil
	}
	if strings.HasPrefix(execution.ContentType, "audio/") || strings.HasPrefix(execution.ContentType, "video/") || strings.HasPrefix(execution.ContentType, "image/") {
		kind, _, _ := strings.Cut(execution.ContentType, "/")
		artifact, err := integration.PersistArtifact(a.db, a.root, row.PrincipalID, row.ClientRequestID, kind, execution.ContentType, execution.NativeBody)
		if err != nil {
			return err
		}
		out.Artifacts = []c.Artifact{artifact}
		return nil
	}
	if row.Operation != string(c.ImageGeneration) && row.Operation != string(c.ImageEdit) {
		return nil
	}
	var response struct {
		Data common.RawMessage `json:"data"`
	}
	if err := common.Unmarshal(out.Result, &response); err != nil {
		return err
	}
	var entries []map[string]common.RawMessage
	switch common.GetJsonType(response.Data) {
	case "array":
		if err := common.Unmarshal(response.Data, &entries); err != nil {
			return err
		}
	case "object":
		var entry map[string]common.RawMessage
		if err := common.Unmarshal(response.Data, &entry); err != nil {
			return err
		}
		entries = []map[string]common.RawMessage{entry}
	default:
		return fmt.Errorf("image response has no data")
	}
	for _, entry := range entries {
		var encoded, url string
		_ = common.Unmarshal(entry["b64_json"], &encoded)
		_ = common.Unmarshal(entry["url"], &url)
		if encoded == "" && url == "" {
			continue
		}
		var body []byte
		var err error
		contentType := ""
		if encoded != "" {
			if int64(len(encoded)) > integration.MediaByteLimit()*4/3+4 {
				return fmt.Errorf("encoded image exceeds limit")
			}
			body, err = base64.StdEncoding.DecodeString(encoded)
		} else {
			if err = service.ValidateSSRFProtectedFetchURL(url); err != nil {
				return err
			}
			req, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if requestErr != nil {
				return requestErr
			}
			resp, fetchErr := service.GetSSRFProtectedHTTPClient().Do(req)
			if fetchErr != nil {
				return fetchErr
			}
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				return fmt.Errorf("image download failed with status %d", resp.StatusCode)
			}
			contentType = resp.Header.Get("Content-Type")
			body, err = io.ReadAll(io.LimitReader(resp.Body, integration.MediaByteLimit()+1))
			resp.Body.Close()
		}
		if err != nil {
			return err
		}
		sniffed := http.DetectContentType(body)
		if !strings.HasPrefix(sniffed, "image/") {
			return fmt.Errorf("provider image content is not a supported image")
		}
		contentType = sniffed
		artifact, err := integration.PersistArtifact(a.db, a.root, row.PrincipalID, row.ClientRequestID, "image", contentType, body)
		if err != nil {
			return err
		}
		duplicate := false
		for _, old := range out.Artifacts {
			if old.Checksum == artifact.Checksum {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out.Artifacts = append(out.Artifacts, artifact)
		}
	}
	if len(out.Artifacts) == 0 {
		return fmt.Errorf("image result has no payload")
	}
	return nil
}
