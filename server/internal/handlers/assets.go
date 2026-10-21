package handlers

import (
	"context"
	"errors"
	"io"
	"net/http"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func (h *Handler) ResolvePoolAsset(ctx context.Context, params serverapi.ResolvePoolAssetParams) (serverapi.ResolvePoolAssetRes, error) {
	content, err := h.services.Assets.ResolveAssetName(ctx, params.PoolId, params.Name)
	if err != nil {
		return apiError(err), nil
	}
	length, digest, body := assetHeaders(content)
	switch content.MediaType {
	case "application/vnd.oci.image.index.v1+json":
		return &serverapi.ResolvePoolAssetOKApplicationVndOciImageIndexV1JSONHeaders{ContentLength: length, DockerContentDigest: digest, Response: serverapi.ResolvePoolAssetOKApplicationVndOciImageIndexV1JSON{Data: body}}, nil
	case "application/vnd.oci.image.manifest.v1+json":
		return &serverapi.ResolvePoolAssetOKApplicationVndOciImageManifestV1JSONHeaders{ContentLength: length, DockerContentDigest: digest, Response: serverapi.ResolvePoolAssetOKApplicationVndOciImageManifestV1JSON{Data: body}}, nil
	case "application/vnd.docker.distribution.manifest.list.v2+json":
		return &serverapi.ResolvePoolAssetOKApplicationVndDockerDistributionManifestListV2JSONHeaders{ContentLength: length, DockerContentDigest: digest, Response: serverapi.ResolvePoolAssetOKApplicationVndDockerDistributionManifestListV2JSON{Data: body}}, nil
	case "application/vnd.docker.distribution.manifest.v2+json":
		return &serverapi.ResolvePoolAssetOKApplicationVndDockerDistributionManifestV2JSONHeaders{ContentLength: length, DockerContentDigest: digest, Response: serverapi.ResolvePoolAssetOKApplicationVndDockerDistributionManifestV2JSON{Data: body}}, nil
	}
	_ = content.Body.Close()
	return nil, errors.New("manifest " + content.Digest + " has media type " + content.MediaType + ", which is not served")
}

func (h *Handler) GetPoolAssetManifest(ctx context.Context, params serverapi.GetPoolAssetManifestParams) (serverapi.GetPoolAssetManifestRes, error) {
	content, err := h.services.Assets.AssetManifest(ctx, params.PoolId, params.Name, params.Digest)
	if err != nil {
		return apiError(err), nil
	}
	length, digest, body := assetHeaders(content)
	switch content.MediaType {
	case "application/vnd.oci.image.index.v1+json":
		return &serverapi.GetPoolAssetManifestOKApplicationVndOciImageIndexV1JSONHeaders{ContentLength: length, DockerContentDigest: digest, Response: serverapi.GetPoolAssetManifestOKApplicationVndOciImageIndexV1JSON{Data: body}}, nil
	case "application/vnd.oci.image.manifest.v1+json":
		return &serverapi.GetPoolAssetManifestOKApplicationVndOciImageManifestV1JSONHeaders{ContentLength: length, DockerContentDigest: digest, Response: serverapi.GetPoolAssetManifestOKApplicationVndOciImageManifestV1JSON{Data: body}}, nil
	case "application/vnd.docker.distribution.manifest.list.v2+json":
		return &serverapi.GetPoolAssetManifestOKApplicationVndDockerDistributionManifestListV2JSONHeaders{ContentLength: length, DockerContentDigest: digest, Response: serverapi.GetPoolAssetManifestOKApplicationVndDockerDistributionManifestListV2JSON{Data: body}}, nil
	case "application/vnd.docker.distribution.manifest.v2+json":
		return &serverapi.GetPoolAssetManifestOKApplicationVndDockerDistributionManifestV2JSONHeaders{ContentLength: length, DockerContentDigest: digest, Response: serverapi.GetPoolAssetManifestOKApplicationVndDockerDistributionManifestV2JSON{Data: body}}, nil
	}
	_ = content.Body.Close()
	return nil, errors.New("manifest " + content.Digest + " has media type " + content.MediaType + ", which is not served")
}

func (h *Handler) GetPoolAssetBlob(ctx context.Context, params serverapi.GetPoolAssetBlobParams) (serverapi.GetPoolAssetBlobRes, error) {
	content, err := h.services.Assets.AssetBlob(ctx, params.PoolId, params.Digest)
	if err != nil {
		return apiError(err), nil
	}
	length, digest, body := assetHeaders(content)
	return &serverapi.GetPoolAssetBlobOKHeaders{ContentLength: length, DockerContentDigest: digest, Response: serverapi.GetPoolAssetBlobOK{Data: body}}, nil
}

// assetHeaders is what every asset response carries: its length, its digest,
// and a body that is cut off rather than completed when its bytes turn out
// not to be that digest's.
func assetHeaders(content services.AssetContent) (serverapi.OptInt64, serverapi.OptString, io.Reader) {
	return serverapi.NewOptInt64(content.Size), serverapi.NewOptString(content.Digest), &abortOnMismatch{body: content.Body}
}

// abortOnMismatch ends the response, rather than the read, when the body
// fails. The status line and headers are on the wire by then, so the encoder's
// error would otherwise be written into the body behind the bytes already
// sent; aborting the handler closes the connection short of Content-Length,
// which the client reads as a broken response — never a complete one whose
// bytes are not the blob's.
type abortOnMismatch struct {
	body io.ReadCloser
}

func (a *abortOnMismatch) Read(p []byte) (int, error) {
	n, err := a.body.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		_ = a.body.Close()
		panic(http.ErrAbortHandler)
	}
	return n, err
}

func (a *abortOnMismatch) Close() error { return a.body.Close() }
