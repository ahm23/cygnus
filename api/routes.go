package api

import (
	"cygnus/config"
	"cygnus/storage"
	"cygnus/wallet"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

func SetupRoutes(app *fiber.App, atlas *wallet.AtlasManager, storageManager *storage.StorageManager, logger *zap.Logger, cfg *config.Config) {
	handler := NewHandler(storageManager, logger, cfg)

	middleware := Middleware{
		atlas.QueryClient,
	}

	// API group with auth middleware
	api := app.Group("/api/v1")
	// api.Use(AuthMiddleware(cfg))
	// api.Use(FileSizeLimitMiddleware(cfg.Storage.MaxUploadSize))

	// File operations
	api.Post("/upload",
		middleware.ValidateSufficientStorage,
		middleware.ValidateStagedFileExists,
		handler.UploadFile)

	// api.Get("/files", handler.ListFiles)
	// api.Get("/files/:id", handler.GetFile)
	// api.Get("/files/:id/download", handler.DownloadFile)
	// api.Delete("/files/:id", handler.DeleteFile)
	// api.Get("/files/stats", handler.GetFileStats)

	// Provider operations
	api.Get("/status", handler.GetStatus)

	// Public endpoints
	app.Get("/health", handler.HealthCheck)
	app.Get("/status", handler.GetStatus) // Public status endpoint
}

// SetupSwagger for API documentation (optional)
func SetupSwagger(app *fiber.App) {
	// Add swagger UI if needed
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
