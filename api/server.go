package api

import (
	"context"
	"cygnus/config"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
)

type API struct {
	port int64
	srv  *fiber.App
	cfg  *config.APIConfig
}

// NewAPI creates a new API instance using the provided API configuration.
func NewAPI(cfg *config.APIConfig) *API {
	srv := fiber.New(fiber.Config{
		AppName:                      "Cygnus DePIN Storage Provider",
		ReadTimeout:                  5 * time.Minute,
		WriteTimeout:                 5 * time.Minute,
		IdleTimeout:                  5 * time.Minute,
		DisablePreParseMultipartForm: true,
		BodyLimit:                    4 * 1024 * 1024 * 1024,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			fmt.Println("ERROR:", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": err.Error(),
			})
		},
	})

	srv.Use(cors.New())

	return &API{
		port: cfg.Port,
		cfg:  cfg,
		srv:  srv,
	}
}

func (a *API) Close() error {
	if a.srv == nil {
		return fmt.Errorf("no server available")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	return a.srv.ShutdownWithContext(shutdownCtx)
}

func (a *API) Serve() {
	addr := fmt.Sprintf(":%d", a.port)
	err := a.srv.Listen(addr)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Println("ERROR STARTING SERVER:", err)
	}
}
