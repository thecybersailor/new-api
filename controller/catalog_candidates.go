package controller

import (
	"github.com/QuantumNous/new-api/model"
	c "github.com/QuantumNous/new-api/pkg/catalogcontract"
)

// Historical candidates only: no route, price or capability is invented.
func catalogCandidateEntries() []model.CatalogMaintenanceEntry {
	return []model.CatalogMaintenanceEntry{
		{Model: "happyhorse/happyhorse-1.1-i2v", DisplayName: "happyhorse/happyhorse-1.1-i2v", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("video_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "happyhorse/happyhorse-1.1-r2v", DisplayName: "happyhorse/happyhorse-1.1-r2v", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("video_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "happyhorse/happyhorse-1.1-t2v", DisplayName: "happyhorse/happyhorse-1.1-t2v", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("video_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "minimax/h3", DisplayName: "minimax/h3", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("video_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "minimax/h3-fast", DisplayName: "minimax/h3-fast", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("video_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "qwen/qwen-image-2.0", DisplayName: "qwen/qwen-image-2.0", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("image_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "qwen/qwen3-asr-flash", DisplayName: "qwen/qwen3-asr-flash", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("audio_transcription"), Config: []byte("{}"), State: "draft"}}},
		{Model: "qwen/qwen3-tts-flash", DisplayName: "qwen/qwen3-tts-flash", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("speech_synthesis"), Config: []byte("{}"), State: "draft"}}},
		{Model: "qwen/qwen3-vl-plus", DisplayName: "qwen/qwen3-vl-plus", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("text_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "qwen/qwen3.5-omni-flash", DisplayName: "qwen/qwen3.5-omni-flash", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("text_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "qwen/qwen3.7-flash", DisplayName: "qwen/qwen3.7-flash", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("text_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "qwen/qwen3.7-max", DisplayName: "qwen/qwen3.7-max", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("text_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "qwen/qwen3.7-plus", DisplayName: "qwen/qwen3.7-plus", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("text_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "qwen/qwen3.7-text-embedding", DisplayName: "qwen/qwen3.7-text-embedding", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("embedding"), Config: []byte("{}"), State: "draft"}}},
		{Model: "qwen/qwen3.8-max", DisplayName: "qwen/qwen3.8-max", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("text_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "qwen/text-embedding-v4", DisplayName: "qwen/text-embedding-v4", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("embedding"), Config: []byte("{}"), State: "draft"}}},
		{Model: "wan/wan2.6-t2v", DisplayName: "wan/wan2.6-t2v", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("video_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "wan/wan2.7-i2v", DisplayName: "wan/wan2.7-i2v", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("video_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "wan/wan2.7-image", DisplayName: "wan/wan2.7-image", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("image_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "wan/wan2.7-r2v", DisplayName: "wan/wan2.7-r2v", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("video_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "wan/wan2.7-t2v", DisplayName: "wan/wan2.7-t2v", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("video_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "wan/wan3.0-video", DisplayName: "wan/wan3.0-video", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("video_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "wan/wan3.0-video-prime", DisplayName: "wan/wan3.0-video-prime", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("video_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "z-ai/glm-5.2", DisplayName: "z-ai/glm-5.2", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("text_generation"), Config: []byte("{}"), State: "draft"}}},
		{Model: "z-ai/glm-5.3", DisplayName: "z-ai/glm-5.3", Operations: []model.CatalogMaintenanceOperation{{Operation: c.Operation("text_generation"), Config: []byte("{}"), State: "draft"}}},
	}
}
