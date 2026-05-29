package api

import (
	"cygnus/atlas"
	"cygnus/config"
	"cygnus/storage"

	"github.com/gofiber/fiber/v2"
)

func (a *API) SetupRoutes(cfg *config.Config, atlas *atlas.AtlasManager, storageManager *storage.StorageManager) {
	handler := NewHandler(storageManager, cfg, atlas)

	api := a.srv.Group("/api/v1")

	api.Get("/health", handler.HealthCheck)
	api.Get("/status", handler.GetStatus)
	api.Get("/files", handler.ListFiles)
	api.Get("/files/:id", handler.GetFile)

	api.Get("/download/:id", handler.DownloadFile)
	api.Post("/upload", handler.UploadFile)
}

// SetupSwagger for API documentation (optional).
func SetupSwagger(app *fiber.App) {
	app.Get("/docs", func(c *fiber.Ctx) error {
		return c.SendString("API Documentation - Add Swagger UI here")
	})

	app.Get("/docs.json", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"openapi": "3.0.0",
			"info": fiber.Map{
				"title":   "DePIN Storage Provider API",
				"version": "1.0.0",
			},
			"paths": fiber.Map{},
		})
	})
}
