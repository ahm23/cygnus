package api

import (
	"strconv"

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

func respondError(c *fiber.Ctx, status int, message string) error {
	return c.Status(status).JSON(types.APIResponse{
		Success: false,
		Error:   message,
	})
}

func respondSuccess(c *fiber.Ctx, data interface{}, message string) error {
	return c.JSON(types.APIResponse{
		Success: true,
		Data:    data,
		Message: message,
	})
}

func parsePositiveInt(value string, fallback int) int {
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return fallback
	}
	return parsed
}

func (h *Handler) UploadFile(c *fiber.Ctx) error {
	fileID := c.FormValue("fid", "")
	if fileID == "" {
		return respondError(c, fiber.StatusBadRequest, "no file id provided")
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		return respondError(c, fiber.StatusBadRequest, "no file uploaded")
	}
	if fileHeader.Size > h.config.APICfg.MaxUploadSize {
		return respondError(c, fiber.StatusBadRequest, "file size exceeds limit")
	}

	metadata, err := h.storageManager.CreateFile(c.Context(), fileID, fileHeader)
	if err != nil {
		h.logger.Error("Failed to upload file", zap.String("file_id", fileID), zap.Error(err))
		return respondError(c, fiber.StatusInternalServerError, err.Error())
	}

	return respondSuccess(c, metadata, "file uploaded successfully")
}

func (h *Handler) ListFiles(c *fiber.Ctx) error {
	page := parsePositiveInt(c.Query("page"), 1)
	pageSize := parsePositiveInt(c.Query("page_size"), 25)

	files, err := h.storageManager.ListFiles(c.Context(), page, pageSize)
	if err != nil {
		h.logger.Error("Failed to list files", zap.Error(err))
		return respondError(c, fiber.StatusInternalServerError, "failed to list files")
	}

	return respondSuccess(c, files, "files retrieved successfully")
}

func (h *Handler) GetFile(c *fiber.Ctx) error {
	fileID := c.Params("id")
	metadata, _, err := h.storageManager.GetFile(c.Context(), fileID)
	if err != nil {
		return respondError(c, fiber.StatusNotFound, err.Error())
	}

	return respondSuccess(c, metadata, "file metadata retrieved successfully")
}

func (h *Handler) DownloadFile(c *fiber.Ctx) error {
	fileID := c.Params("id")
	metadata, file, err := h.storageManager.GetFile(c.Context(), fileID)
	if err != nil {
		return respondError(c, fiber.StatusNotFound, err.Error())
	}
	defer file.Close()

	c.Set(fiber.HeaderContentDisposition, "attachment; filename=\""+metadata.FileName+"\"")
	c.Set(fiber.HeaderContentType, fiber.MIMEOctetStream)
	return c.SendStream(file, int(metadata.Size))
}

func (h *Handler) DeleteFile(c *fiber.Ctx) error {
	fileID := c.Params("id")
	if err := h.storageManager.DeleteFile(c.Context(), fileID); err != nil {
		h.logger.Error("Failed to delete file", zap.String("file_id", fileID), zap.Error(err))
		return respondError(c, fiber.StatusInternalServerError, err.Error())
	}

	return respondSuccess(c, fiber.Map{"fid": fileID}, "file deleted successfully")
}

func (h *Handler) HealthCheck(c *fiber.Ctx) error {
	return respondSuccess(c, fiber.Map{"status": "ok"}, "provider is healthy")
}

func (h *Handler) GetStatus(c *fiber.Ctx) error {
	status, err := h.storageManager.GetStatus()
	if err != nil {
		h.logger.Error("Failed to get provider status", zap.Error(err))
		return respondError(c, fiber.StatusInternalServerError, "failed to get provider status")
	}

	return respondSuccess(c, status, "provider status retrieved successfully")
}
