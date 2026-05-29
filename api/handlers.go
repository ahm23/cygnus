package api

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strconv"
	"strings"

	storagetypes "atlas/x/storage/types"

	"cygnus/atlas"
	"cygnus/config"
	"cygnus/storage"
	"cygnus/types"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"
)

type Handler struct {
	storageManager *storage.StorageManager
	config         *config.Config
	atlas          *atlas.AtlasManager
}

func NewHandler(storageManager *storage.StorageManager, cfg *config.Config, atlas *atlas.AtlasManager) *Handler {
	return &Handler{
		storageManager: storageManager,
		config:         cfg,
		atlas:          atlas,
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
	// Extract the multipart boundary from the Content-Type header.
	contentType := string(c.Request().Header.ContentType())
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return respondError(c, fiber.StatusBadRequest, "invalid content-type")
	}
	boundary := params["boundary"]
	if boundary == "" {
		return respondError(c, fiber.StatusBadRequest, "missing multipart boundary")
	}

	// Read the raw body bytes from fasthttp's internal buffer.
	// With DisablePreParseMultipartForm: true the body is NOT pre-parsed,
	// so this avoids the double pass through MultipartForm() + temp files.
	body := c.Request().Body()
	if len(body) == 0 {
		return respondError(c, fiber.StatusBadRequest, "empty request body")
	}

	// Stream-parse the multipart body in a single pass.
	reader := multipart.NewReader(bytes.NewReader(body), boundary)

	var fileID string
	var fileName string
	var fileSize int64
	var filePart io.Reader

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return respondError(c, fiber.StatusBadRequest, "failed to parse multipart body")
		}

		formName := part.FormName()
		if formName == "fid" {
			fidBytes := bytes.Buffer{}
			_, _ = fidBytes.ReadFrom(part)
			fileID = strings.TrimSpace(fidBytes.String())
		} else if formName == "file" {
			fileName = part.FileName()
			// part.Size is unavailable in older Go versions; read from part headers instead.
			if cl := part.Header.Get("Content-Length"); cl != "" {
				if n, parseErr := strconv.ParseInt(cl, 10, 64); parseErr == nil && n > 0 {
					fileSize = n
				}
			}
			filePart = part
		}
		// ignore other form fields
	}

	if fileID == "" {
		return respondError(c, fiber.StatusBadRequest, "no file id provided")
	}
	if filePart == nil {
		return respondError(c, fiber.StatusBadRequest, "no file uploaded")
	}
	if fileSize > h.config.APICfg.MaxUploadSize {
		return respondError(c, fiber.StatusBadRequest, "file size exceeds limit")
	}

	// Validate that the file exists as a staged entry on-chain.
	if h.atlas != nil && h.atlas.QueryClients.Storage != nil {
		req := storagetypes.QueryFileRequest{Fid: fileID}
		res, queryErr := h.atlas.QueryClients.Storage.File(c.Context(), &req)
		if queryErr != nil {
			return respondError(c, fiber.StatusBadRequest, fmt.Sprintf("unable to find staged file with id %s", fileID))
		}
		if res.File == nil {
			return respondError(c, fiber.StatusBadRequest, fmt.Sprintf("staged file metadata not found for id %s", fileID))
		}
		if int32(len(res.File.Providers)) >= res.File.Replicas {
			return respondError(c, fiber.StatusConflict, fmt.Sprintf("file %s already has max replicas", fileID))
		}
	}

	metadata, err := h.storageManager.ClaimFile(c.Context(), fileID, fileName, filePart, fileSize)
	if err != nil {
		log.Error().Str("file_id", fileID).Err(err).Msg("Failed to upload file")
		return respondError(c, fiber.StatusInternalServerError, err.Error())
	}

	return respondSuccess(c, metadata, "file uploaded successfully")
}

func (h *Handler) ListFiles(c *fiber.Ctx) error {
	page := parsePositiveInt(c.Query("page"), 1)
	pageSize := parsePositiveInt(c.Query("page_size"), 25)

	files, err := h.storageManager.ListFiles(c.Context(), page, pageSize)
	if err != nil {
		log.Error().Err(err).Msg("Failed to list files")
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
	metadata, err := h.storageManager.GetFileMetadata(c.Context(), fileID)
	if err != nil {
		return respondError(c, fiber.StatusNotFound, err.Error())
	}
	filePath, err := h.storageManager.GetFilePath(fileID)
	if err != nil {
		return respondError(c, fiber.StatusNotFound, err.Error())
	}

	if err != nil {
		return respondError(c, fiber.StatusInternalServerError, err.Error())
	}

	c.Set(fiber.HeaderContentDisposition, "attachment; filename=\""+metadata.FileName+"\"")
	return c.SendFile(filePath)
}

func (h *Handler) HealthCheck(c *fiber.Ctx) error {
	return respondSuccess(c, fiber.Map{"status": "ok"}, "provider is healthy")
}

func (h *Handler) GetStatus(c *fiber.Ctx) error {
	status, err := h.storageManager.GetStatus()
	if err != nil {
		log.Error().Err(err).Msg("Failed to get provider status")
		return respondError(c, fiber.StatusInternalServerError, "failed to get provider status")
	}

	return respondSuccess(c, status, "provider status retrieved successfully")
}
