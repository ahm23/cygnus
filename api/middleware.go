package api

import (
	"cygnus/types"
	storagetypes "nebulix/x/storage/types"

	"github.com/gofiber/fiber/v2"
)

type Middleware struct {
	AtlasQueryClients *types.QueryClients
}

func (m *Middleware) ValidateStagedFileExists(c *fiber.Ctx) error {
	fileId := c.FormValue("fid")
	if fileId == "" {
		return c.Status(400).SendString("Missing file ID in request header!")
	}
	req := storagetypes.QueryFileRequest{Fid: fileId}
	res, err := m.AtlasQueryClients.Storage.File(c.Context(), &req)
	if err != nil {
		return c.Status(400).SendString("Unable to find staged file with ID " + fileId)
	} else if int64(len(res.File.Providers)) >= res.File.Replicas {
		return c.Status(409).SendString("File with ID " + fileId + "already has max. replicas stored accross the network.")
	} else {
		return c.Next()
	}
}

func (m *Middleware) ValidateSufficientStorage(c *fiber.Ctx) error {
	// [TODO]: return error 507 if not enough

	return c.Next()
}
