package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	storagetypes "atlas/x/storage/types"

	"cygnus/atlas"
	"cygnus/config"
	"cygnus/storage"
	"cygnus/types"

	"github.com/rs/zerolog/log"
)

// UploadServer is a lightweight net/http server that streams multipart uploads
// directly from the TCP connection, bypassing fasthttp's in-memory body buffering.
type UploadServer struct {
	storageManager *storage.StorageManager
	atlas          *atlas.AtlasManager
	cfg            *config.APIConfig
	server         *http.Server
}

func NewUploadServer(sm *storage.StorageManager, am *atlas.AtlasManager, cfg *config.APIConfig) *UploadServer {
	return &UploadServer{
		storageManager: sm,
		atlas:          am,
		cfg:            cfg,
	}
}

// Serve starts the upload listener on the configured port. Blocks until
// the server stops. Call with go.
func (us *UploadServer) Serve(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", us.cfg.UploadListenPort)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/upload", us.handleUpload)

	us.server = &http.Server{
		Addr:        addr,
		Handler:     corsMiddleware(mux),
		ReadTimeout: 10 * time.Minute,
	}

	log.Info().Str("addr", addr).Msg("Streaming upload listener starting")
	return us.server.ListenAndServe()
}

func (us *UploadServer) Shutdown(ctx context.Context) error {
	if us.server != nil {
		return us.server.Shutdown(ctx)
	}
	return nil
}

// corsMiddleware wraps a handler with permissive CORS headers for the upload endpoint.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (us *UploadServer) respond(w http.ResponseWriter, status int, resp types.APIResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func (us *UploadServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	remoteIP := r.RemoteAddr
	cl := r.Header.Get("Content-Length")
	if cl == "" {
		cl = "unknown"
	}

	if r.Method != http.MethodPost {
		log.Warn().Str("remote", remoteIP).Str("method", r.Method).Msg("Upload: rejected non-POST")
		us.respond(w, http.StatusMethodNotAllowed, types.APIResponse{Success: false, Error: "method not allowed"})
		return
	}

	log.Info().Str("remote", remoteIP).Str("cl", cl).Msg("Upload: incoming request")

	// --- extract multipart boundary from Content-Type ---
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/form-data") {
		log.Warn().Str("remote", remoteIP).Str("content_type", r.Header.Get("Content-Type")).Msg("Upload: invalid content-type")
		us.respond(w, http.StatusBadRequest, types.APIResponse{Success: false, Error: "invalid content-type"})
		return
	}
	boundary := params["boundary"]
	if boundary == "" {
		log.Warn().Str("remote", remoteIP).Msg("Upload: missing multipart boundary")
		us.respond(w, http.StatusBadRequest, types.APIResponse{Success: false, Error: "missing multipart boundary"})
		return
	}

	log.Info().Str("remote", remoteIP).Msg("Upload: boundary parsed, starting stream parse")

	// --- enforce max upload size at the body level ---
	maxSize := us.cfg.MaxUploadSize
	if maxSize <= 0 {
		maxSize = 4 * 1024 * 1024 * 1024
	}
	src := http.MaxBytesReader(w, r.Body, maxSize)

	// --- stream-parse multipart in a single pass ---
	reader := multipart.NewReader(src, boundary)

	var fileID string
	var fileName string
	var fileSize int64
	var filePart io.Reader

	for {
		part, partErr := reader.NextPart()
		if partErr == io.EOF {
			break
		}
		if partErr != nil {
			log.Warn().Str("remote", remoteIP).Err(partErr).Msg("Upload: failed to parse multipart body")
			us.respond(w, http.StatusBadRequest, types.APIResponse{Success: false, Error: "failed to parse multipart body"})
			return
		}

		formName := part.FormName()
		switch formName {
		case "fid":
			buf := bytes.Buffer{}
			_, _ = buf.ReadFrom(part)
			fileID = strings.TrimSpace(buf.String())
			log.Info().Str("file_id", fileID).Msg("Upload: parsed fid form field")
		case "file":
			fileName = part.FileName()
			if cl := part.Header.Get("Content-Length"); cl != "" {
				if n, parseErr := strconv.ParseInt(cl, 10, 64); parseErr == nil && n > 0 {
					fileSize = n
				}
			}
			filePart = part
			log.Info().
				Str("file_id", fileID).
				Str("file_name", fileName).
				Int64("file_size", fileSize).
				Msg("Upload: found file part, starting streaming write")
			goto foundFile // break out of the loop without calling NextPart() again
		}
	}
	// fell through — file part not found
	log.Warn().Str("remote", remoteIP).Msg("Upload: no file part found in multipart body")
	us.respond(w, http.StatusBadRequest, types.APIResponse{Success: false, Error: "no file uploaded"})
	return

foundFile:
	if fileID == "" {
		log.Warn().Str("remote", remoteIP).Msg("Upload: file part found but fid is missing")
		us.respond(w, http.StatusBadRequest, types.APIResponse{Success: false, Error: "no file id provided"})
		return
	}

	// --- validate staged file on-chain ---
	if us.atlas != nil && us.atlas.QueryClients.Storage != nil {
		log.Info().Str("file_id", fileID).Msg("Upload: validating staged file on chain")
		req := storagetypes.QueryFileRequest{Fid: fileID}
		res, queryErr := us.atlas.QueryClients.Storage.File(r.Context(), &req)
		if queryErr != nil {
			log.Warn().Str("file_id", fileID).Err(queryErr).Msg("Upload: chain query failed for staged file")
			us.respond(w, http.StatusBadRequest, types.APIResponse{Success: false, Error: fmt.Sprintf("unable to find staged file with id %s", fileID)})
			return
		}
		if res.File == nil {
			log.Warn().Str("file_id", fileID).Msg("Upload: staged file not found on chain")
			us.respond(w, http.StatusBadRequest, types.APIResponse{Success: false, Error: fmt.Sprintf("staged file metadata not found for id %s", fileID)})
			return
		}
		if int32(len(res.File.Providers)) >= res.File.Replicas {
			log.Warn().Str("file_id", fileID).Msg("Upload: file already has max replicas")
			us.respond(w, http.StatusConflict, types.APIResponse{Success: false, Error: fmt.Sprintf("file %s already has max replicas", fileID)})
			return
		}
		log.Info().Str("file_id", fileID).Msg("Upload: staged file validated")
	}

	// --- stream file data to disk + build merkle tree ---
	log.Info().Str("file_id", fileID).Int64("size", fileSize).Msg("Upload: streaming to ClaimFile")
	metadata, err := us.storageManager.ClaimFile(r.Context(), fileID, fileName, filePart, fileSize)
	if err != nil {
		log.Error().Str("file_id", fileID).Err(err).Msg("Upload: ClaimFile failed")
		us.respond(w, http.StatusInternalServerError, types.APIResponse{Success: false, Error: err.Error()})
		return
	}

	log.Info().
		Str("file_id", fileID).
		Str("file_name", metadata.FileName).
		Int64("size", metadata.Size).
		Int("chunks", metadata.Chunks).
		Str("merkle_root", metadata.MerkleRoot).
		Msg("Upload: file uploaded successfully")
	us.respond(w, http.StatusOK, types.APIResponse{Success: true, Data: metadata, Message: "file uploaded successfully"})
}
