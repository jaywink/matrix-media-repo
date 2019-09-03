package download_controller

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/disintegration/imaging"
	"github.com/patrickmn/go-cache"
	"github.com/sirupsen/logrus"
	"github.com/turt2live/matrix-media-repo/common"
	"github.com/turt2live/matrix-media-repo/common/config"
	"github.com/turt2live/matrix-media-repo/common/globals"
	"github.com/turt2live/matrix-media-repo/controllers/quarantine_controller"
	"github.com/turt2live/matrix-media-repo/internal_cache"
	"github.com/turt2live/matrix-media-repo/storage"
	"github.com/turt2live/matrix-media-repo/storage/datastore"
	"github.com/turt2live/matrix-media-repo/types"
	"github.com/turt2live/matrix-media-repo/util"
)

var localCache = cache.New(30*time.Second, 60*time.Second)

func GetMedia(origin string, mediaId string, downloadRemote bool, blockForMedia bool, ctx context.Context, log *logrus.Entry) (*types.MinimalMedia, error) {
	cacheKey := fmt.Sprintf("%s/%s?r=%t&b=%t", origin, mediaId, downloadRemote, blockForMedia)
	v, _, err := globals.DefaultRequestGroup.Do(cacheKey, func() (interface{}, error) {
		var media *types.Media
		var minMedia *types.MinimalMedia
		var err error
		if blockForMedia {
			media, err = FindMediaRecord(origin, mediaId, downloadRemote, ctx, log)
			if media != nil {
				minMedia = &types.MinimalMedia{
					Origin:      media.Origin,
					MediaId:     media.MediaId,
					ContentType: media.ContentType,
					UploadName:  media.UploadName,
					SizeBytes:   media.SizeBytes,
					Stream:      nil, // we'll populate this later if we need to
					KnownMedia:  media,
				}
			}
		} else {
			minMedia, err = FindMinimalMediaRecord(origin, mediaId, downloadRemote, ctx, log)
			if minMedia != nil {
				media = minMedia.KnownMedia
			}
		}
		if err != nil {
			return nil, err
		}
		if minMedia == nil {
			log.Warn("Unexpected error while fetching media: no minimal media record")
			return nil, common.ErrMediaNotFound
		}
		if media == nil && blockForMedia {
			log.Warn("Unexpected error while fetching media: no regular media record (block for media in place)")
			return nil, common.ErrMediaNotFound
		}

		// if we have a known media record, we might as well set it
		// if we don't, this won't do anything different
		minMedia.KnownMedia = media

		if media != nil {
			if media.Quarantined {
				log.Warn("Quarantined media accessed")

				if config.Get().Quarantine.ReplaceDownloads {
					log.Info("Replacing thumbnail with a quarantined one")

					img, err := quarantine_controller.GenerateQuarantineThumbnail(512, 512)
					if err != nil {
						return nil, err
					}

					data := &bytes.Buffer{}
					imaging.Encode(data, img, imaging.PNG)
					return &types.MinimalMedia{
						// Lie about all the details
						Stream:      util.BufferToStream(data),
						ContentType: "image/png",
						UploadName:  "quarantine.png",
						SizeBytes:   int64(data.Len()),
						MediaId:     mediaId,
						Origin:      origin,
						KnownMedia:  media,
					}, nil
				}

				return nil, common.ErrMediaQuarantined
			}

			err = storage.GetDatabase().GetMetadataStore(ctx, log).UpsertLastAccess(media.Sha256Hash, util.NowMillis())
			if err != nil {
				logrus.Warn("Failed to upsert the last access time: ", err)
			}

			localCache.Set(origin+"/"+mediaId, media, cache.DefaultExpiration)
			internal_cache.Get().IncrementDownloads(media.Sha256Hash)

			cached, err := internal_cache.Get().GetMedia(media, log)
			if err != nil {
				return nil, err
			}
			if cached != nil && cached.Contents != nil {
				minMedia.Stream = util.BufferToStream(cached.Contents)
				return minMedia, nil
			}
		}

		if minMedia.Stream != nil {
			log.Info("Returning minimal media record with a viable stream")
			return minMedia, nil
		}

		if media == nil {
			log.Error("Failed to locate media")
			return nil, errors.New("failed to locate media")
		}

		log.Info("Reading media from disk")
		mediaStream, err := datastore.DownloadStream(ctx, log, media.DatastoreId, media.Location)
		if err != nil {
			return nil, err
		}

		minMedia.Stream = mediaStream
		return minMedia, nil
	}, func(v interface{}, count int, err error) []interface{} {
		if err != nil {
			return nil
		}

		rv := v.(*types.MinimalMedia)
		vals := make([]interface{}, 0)
		streams := util.CloneReader(rv.Stream, count)

		for i := 0; i < count; i++ {
			if rv.KnownMedia != nil {
				internal_cache.Get().IncrementDownloads(rv.KnownMedia.Sha256Hash)
			}
			vals = append(vals, &types.MinimalMedia{
				Origin:      rv.Origin,
				MediaId:     rv.MediaId,
				UploadName:  rv.UploadName,
				ContentType: rv.ContentType,
				SizeBytes:   rv.SizeBytes,
				KnownMedia:  rv.KnownMedia,
				Stream:      streams[i],
			})
		}

		return vals
	})

	var value *types.MinimalMedia
	if v != nil {
		value = v.(*types.MinimalMedia)
	}

	return value, err
}

func FindMinimalMediaRecord(origin string, mediaId string, downloadRemote bool, ctx context.Context, log *logrus.Entry) (*types.MinimalMedia, error) {
	db := storage.GetDatabase().GetMediaStore(ctx, log)

	var media *types.Media
	item, found := localCache.Get(origin + "/" + mediaId)
	if found {
		media = item.(*types.Media)
	} else {
		log.Info("Getting media record from database")
		dbMedia, err := db.Get(origin, mediaId)
		if err != nil {
			if err == sql.ErrNoRows {
				if util.IsServerOurs(origin) {
					log.Warn("Media not found")
					return nil, common.ErrMediaNotFound
				}
			}

			if !downloadRemote {
				log.Warn("Remote media not being downloaded")
				return nil, common.ErrMediaNotFound
			}

			mediaChan := getResourceHandler().DownloadRemoteMedia(origin, mediaId, true)
			defer close(mediaChan)

			result := <-mediaChan
			if result.err != nil {
				return nil, result.err
			}
			if result.stream == nil {
				log.Info("No stream returned from remote download - attempting to create one")
				if result.media == nil {
					log.Error("Fatal error: No stream and no media. Cannot acquire a stream for media")
					return nil, errors.New("no stream available")
				}

				stream, err := datastore.DownloadStream(ctx, log, result.media.DatastoreId, result.media.Location)
				if err != nil {
					return nil, err
				}

				result.stream = stream
			}
			return &types.MinimalMedia{
				Origin:      origin,
				MediaId:     mediaId,
				ContentType: result.contentType,
				UploadName:  result.filename,
				SizeBytes:   -1, // unknown
				Stream:      result.stream,
				KnownMedia:  nil, // unknown
			}, nil
		} else {
			media = dbMedia
		}
	}

	if media == nil {
		log.Warn("Despite all efforts, a media record could not be found")
		return nil, common.ErrMediaNotFound
	}

	mediaStream, err := datastore.DownloadStream(ctx, log, media.DatastoreId, media.Location)
	if err != nil {
		return nil, err
	}

	return &types.MinimalMedia{
		Origin:      media.Origin,
		MediaId:     media.MediaId,
		ContentType: media.ContentType,
		UploadName:  media.UploadName,
		SizeBytes:   media.SizeBytes,
		Stream:      mediaStream,
		KnownMedia:  media,
	}, nil
}

func FindMediaRecord(origin string, mediaId string, downloadRemote bool, ctx context.Context, log *logrus.Entry) (*types.Media, error) {
	cacheKey := origin + "/" + mediaId
	v, _, err := globals.DefaultRequestGroup.DoWithoutPost(cacheKey, func() (interface{}, error) {
		db := storage.GetDatabase().GetMediaStore(ctx, log)

		var media *types.Media
		item, found := localCache.Get(cacheKey)
		if found {
			media = item.(*types.Media)
		} else {
			log.Info("Getting media record from database")
			dbMedia, err := db.Get(origin, mediaId)
			if err != nil {
				if err == sql.ErrNoRows {
					if util.IsServerOurs(origin) {
						log.Warn("Media not found")
						return nil, common.ErrMediaNotFound
					}
				}

				if !downloadRemote {
					log.Warn("Remote media not being downloaded")
					return nil, common.ErrMediaNotFound
				}

				mediaChan := getResourceHandler().DownloadRemoteMedia(origin, mediaId, true)
				defer close(mediaChan)

				result := <-mediaChan
				if result.err != nil {
					return nil, result.err
				}
				media = result.media
			} else {
				media = dbMedia
			}
		}

		if media == nil {
			log.Warn("Despite all efforts, a media record could not be found")
			return nil, common.ErrMediaNotFound
		}

		return media, nil
	})

	var value *types.Media
	if v != nil {
		value = v.(*types.Media)
	}

	return value, err
}