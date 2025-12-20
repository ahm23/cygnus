package api

import (
	"context"
	"cygnus/config"
	"cygnus/storage"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
)

type API struct {
	port int64
	srv  *fiber.App
	cfg  *config.APIConfig
}

// NewAPI creates a new API instance using the provided API configuration.
func NewAPI(cfg *config.APIConfig) *API {
	return &API{
		port: cfg.Port,
		cfg:  cfg,
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

// p *proofs.Prover, wallet *wallet.Wallet, chunkSize int64, myIp string
func (a *API) Serve(sm *storage.StorageManager) {
	// defer log.Info().Msg("API module stopped")

	SetupRoutes(a.srv)

	// [TODO]: alow cors
	// Create Fiber app
	a.srv = fiber.New(fiber.Config{
		AppName: "Cygnus DePIN Storage Provider",
		// ReadTimeout:  cfg.Server.ReadTimeout,
		// WriteTimeout: cfg.Server.WriteTimeout,
		// IdleTimeout:  cfg.Server.IdleTimeout,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			// logger.Error("HTTP error",
			// 	zap.String("path", c.Path()),
			// 	zap.String("method", c.Method()),
			// 	zap.Error(err))
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "Internal server error",
			})
		},
	})

	err := a.srv.Listen(fmt.Sprintf(":%s", a.cfg.Port))
	if err != nil {
		if !errors.Is(err, http.ErrServerClosed) {
			// log.Warn().Err(err)
			return
		}
	}
}
