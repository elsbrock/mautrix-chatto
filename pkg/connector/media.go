package connector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoclient"
	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
)

// defaultChunkSize is used when the server does not announce max_chunk_size.
const defaultChunkSize = 4 << 20

// uploadLimit caches the server's upload limit. It lives on the Connector
// because one bridge serves one Chatto server.
type uploadLimit struct {
	mu      sync.Mutex
	size    int64
	fetched time.Time
}

// maxUploadSize returns Chatto's upload limit, cached for an hour.
func (c *ChattoClient) maxUploadSize(ctx context.Context) int64 {
	limit := &c.Main.uploadLimit
	limit.mu.Lock()
	defer limit.mu.Unlock()
	if limit.size > 0 && time.Since(limit.fetched) < time.Hour {
		return limit.size
	}
	limit.fetched = time.Now()
	resp, err := c.api.Server.GetRuntimeConfig(ctx, connect.NewRequest(&apiv1.GetRuntimeConfigRequest{}))
	if err != nil || resp.Msg.GetRuntime().GetMaxUploadSize() <= 0 {
		if limit.size == 0 {
			limit.size = defaultMaxUploadSize
		}
		return limit.size
	}
	limit.size = resp.Msg.GetRuntime().GetMaxUploadSize()
	return limit.size
}

var ErrFileTooLarge = bridgev2.WrapErrorInStatus(errors.New("file is larger than the Chatto server allows")).
	WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true)

// uploadMatrixMedia downloads a Matrix file and uploads it to Chatto in
// chunks, returning the asset ID to attach.
func (c *ChattoClient) uploadMatrixMedia(ctx context.Context, api *chattoclient.Client, roomID string, content *event.MessageEventContent) (string, error) {
	limit := c.maxUploadSize(ctx)
	if content.Info != nil && content.Info.Size > 0 && int64(content.Info.Size) > limit {
		return "", ErrFileTooLarge
	}
	uri := content.URL
	if content.File != nil {
		uri = content.File.URL
	}
	data, err := c.Main.Bridge.Bot.DownloadMedia(ctx, uri, content.File)
	if err != nil {
		return "", bridgev2.WrapErrorInStatus(fmt.Errorf("failed to download media from Matrix: %w", err)).WithErrorAsMessage()
	}
	if int64(len(data)) > limit {
		return "", ErrFileTooLarge
	}
	filename := content.FileName
	if filename == "" {
		filename = content.Body
	}
	mimeType := ""
	if content.Info != nil {
		mimeType = content.Info.MimeType
	}
	if filename == "" {
		filename = "file"
		if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
			filename += exts[0]
		}
	}
	return c.uploadAsset(ctx, api, roomID, filename, mimeType, data)
}

// uploadAsset runs Chatto's chunked upload protocol.
func (c *ChattoClient) uploadAsset(ctx context.Context, api *chattoclient.Client, roomID, filename, contentType string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	created, err := api.AssetUploads.CreateUpload(ctx, connect.NewRequest(&apiv1.CreateUploadRequest{
		RoomId:      roomID,
		Filename:    filename,
		ContentType: contentType,
		Size:        int64(len(data)),
		Sha256:      hex.EncodeToString(sum[:]),
	}))
	if err != nil {
		return "", wrapSendError(err)
	}
	upload := created.Msg.GetUpload()
	chunkSize := int64(upload.GetMaxChunkSize())
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	offset := upload.GetCommittedOffset()
	for offset < int64(len(data)) {
		end := min(offset+chunkSize, int64(len(data)))
		chunk := data[offset:end]
		chunkSum := sha256.Sum256(chunk)
		resp, err := api.AssetUploads.UploadChunk(ctx, connect.NewRequest(&apiv1.UploadChunkRequest{
			UploadId:    upload.GetUploadId(),
			Offset:      offset,
			Content:     bytes.Clone(chunk),
			ChunkSha256: hex.EncodeToString(chunkSum[:]),
		}))
		if err != nil {
			cancelUpload(ctx, api, upload.GetUploadId())
			return "", wrapSendError(err)
		}
		next := resp.Msg.GetUpload().GetCommittedOffset()
		if next <= offset {
			cancelUpload(ctx, api, upload.GetUploadId())
			return "", fmt.Errorf("chatto upload made no progress at offset %d", offset)
		}
		offset = next
	}
	done, err := api.AssetUploads.CompleteUpload(ctx, connect.NewRequest(&apiv1.CompleteUploadRequest{UploadId: upload.GetUploadId()}))
	if err != nil {
		return "", wrapSendError(err)
	}
	assetID := done.Msg.GetUpload().GetAssetId()
	if assetID == "" {
		assetID = done.Msg.GetAsset().GetId()
	}
	if assetID == "" {
		return "", errors.New("chatto upload completed without an asset ID")
	}
	return assetID, nil
}

func cancelUpload(ctx context.Context, api *chattoclient.Client, uploadID string) {
	_, err := api.AssetUploads.CancelUpload(context.WithoutCancel(ctx), connect.NewRequest(&apiv1.CancelUploadRequest{UploadId: uploadID}))
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).Msg("Failed to cancel upload")
	}
}
