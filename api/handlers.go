package api

import (
	"cygnus/config"
	"cygnus/storage"
	"cygnus/types"

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
	// download file into memory
	fileHeader, err := c.FormFile("file")
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(types.APIResponse{
			Success: false,
			Error:   "No file uploaded",
		})
	}

	// check file size limit
	if fileHeader.Size > h.config.APICfg.MaxUploadSize {
		return c.Status(fiber.StatusBadRequest).JSON(types.APIResponse{
			Success: false,
			Error:   "File size exceeds limit",
		})
	}

	// Upload file
	metadata, err := h.storageManager.ProveFile(c.Context(), fileHeader, &req)
	if err != nil {
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
