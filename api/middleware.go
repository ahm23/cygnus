package api

import (
	storagetypes "atlas/x/storage/types"
	"cygnus/config"
	"cygnus/storage"
	"cygnus/types"

	"github.com/gofiber/fiber/v2"
)

type Middleware struct {
	AtlasQueryClients *types.QueryClients
	StorageManager    *storage.StorageManager
	Config            *config.Config
}

func (m *Middleware) jsonError(c *fiber.Ctx, status int, message string) error {
	return c.Status(status).JSON(types.APIResponse{
		Success: false,
		Error:   message,
	})
}

func (m *Middleware) ValidateStagedFileExists(c *fiber.Ctx) error {
	fileID := c.FormValue("fid")
	if fileID == "" {
		return m.jsonError(c, fiber.StatusBadRequest, "missing file ID")
	}
	if m.AtlasQueryClients == nil || m.AtlasQueryClients.Storage == nil {
		return m.jsonError(c, fiber.StatusServiceUnavailable, "storage query client unavailable")
	}

	req := storagetypes.QueryFileRequest{Fid: fileID}
	res, err := m.AtlasQueryClients.Storage.File(c.Context(), &req)
	if err != nil {
		return m.jsonError(c, fiber.StatusBadRequest, "unable to find staged file with ID "+fileID)
	}
	if res.File == nil {
		return m.jsonError(c, fiber.StatusBadRequest, "staged file metadata not found")
	}
	if int32(len(res.File.Providers)) >= res.File.Replicas {
		return m.jsonError(c, fiber.StatusConflict, "file with ID "+fileID+" already has max replicas stored across the network")
	}

	return c.Next()
}

func (m *Middleware) ValidateSufficientStorage(c *fiber.Ctx) error {
	if m.StorageManager == nil {
		return m.jsonError(c, fiber.StatusServiceUnavailable, "storage manager unavailable")
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		return m.jsonError(c, fiber.StatusBadRequest, "no file uploaded")
	}

	ok, _, err := m.StorageManager.HasCapacityFor(c.Context(), fileHeader.Size)
	if err != nil {
		return m.jsonError(c, fiber.StatusInternalServerError, "failed to calculate provider capacity")
	}
	if !ok {
		return m.jsonError(c, fiber.StatusInsufficientStorage, "provider does not have enough remaining storage")
	}

	return c.Next()
}
