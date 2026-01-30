package api

import (
	"cygnus/config"
	"cygnus/storage"
	"cygnus/types"
	"fmt"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

type Handler struct {
	storageManager *storage.StorageManager
	logger         *zap.Logger
	config         *config.Config
}

func NewHandler(storageManager *storage.StorageManager, logger *zap.Logger, cfg *config.Config) *Handler {
	return &Handler{
		storageManager: storageManager,
		logger:         logger,
		config:         cfg,
	}
}

func (h *Handler) UploadFile(c *fiber.Ctx) error {
	// ---- Request Validation ---- //
	// REQUIRED: fileId
	fileId := c.FormValue("fid", "")
	if fileId == "" {
		return c.Status(fiber.StatusBadRequest).JSON(types.APIResponse{
			Success: false,
			Error:   "No file id provided",
		})
	}
	// REQUIRED: file
	fileHeader, err := c.FormFile("file")
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(types.APIResponse{
			Success: false,
			Error:   "No file uploaded",
		})
	}
	// REQUIRED: file size < MaxUploadSize
	if fileHeader.Size > h.config.APICfg.MaxUploadSize {
		return c.Status(fiber.StatusBadRequest).JSON(types.APIResponse{
			Success: false,
			Error:   "File size exceeds limit",
		})
	}

	// ---- Request Handling ---- //
	metadata, err := h.storageManager.CreateFile(c.Context(), fileId, fileHeader)
	if err != nil {
		fmt.Println(err)
		h.logger.Error("Failed to upload file", zap.Error(err))
		return c.Status(fiber.StatusInternalServerError).JSON(types.APIResponse{
			Success: false,
			Error:   "Failed to upload file",
		})
	}

	return c.JSON(types.APIResponse{
		Success: true,
		Data:    metadata,
		Message: "File uploaded successfully",
	})
}
