package api

import (
	"cygnus/types"

	"github.com/gofiber/fiber/v2"
)

type Middleware struct {
	AtlasQueryClients types.QueryClients
}

func (m *Middleware) ValidateStagedFileExists(c *fiber.Ctx) error {
	fileId := c.Get("FileId")
	if fileId == "" {
		return c.Status(400).SendString("Missing file ID in request header!")
	}

	file, err := m.AtlasQueryClients.Storage.File(c)
	if err != nil {
		return c.Status(400).SendString("Unable to find staged file with ID " + fileId)
	} else if len(file.Providers) >= file.Replicas {
		return c.Status(409).SendString("File with ID " + fileId + "already has max. replicas stored accross the network.")
	} else {
		return c.Next()
	}
}

func (m *Middleware) ValidateSufficientStorage(c *fiber.Ctx) error {
	// [TODO]: return error 507 if not enough

	return c.Next()
}
