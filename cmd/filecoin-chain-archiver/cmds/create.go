package cmds

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/filecoin-project/filecoin-chain-archiver/pkg/config"
	"github.com/filecoin-project/filecoin-chain-archiver/pkg/consensus"
	"github.com/filecoin-project/filecoin-chain-archiver/pkg/export"
	"github.com/filecoin-project/filecoin-chain-archiver/pkg/nodelocker/client"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/klauspost/compress/zstd"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/urfave/cli/v2"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/lotus/api"
)

func Compress(in io.Reader, out io.Writer) error {
	enc, err := zstd.NewWriter(out)
	if err != nil {
		return err
	}
	_, err = io.Copy(enc, in)
	if err != nil {
		enc.Close()
		return err
	}
	return enc.Close()
}

type snapshotInfo struct {
	digest         string
	size           int64
	filename       string
	latestIndex    string
	latestLocation string
}

type snapshotReader struct {
	reader io.Reader
	errCh  chan error
}

func (sr *snapshotReader) Read(p []byte) (n int, err error) {
	n, _ = sr.reader.Read(p)
	select {
	case err := <-sr.errCh:
		if err != nil {
			return n, err
		}
		return n, io.EOF
	default:
	}
	return n, nil
}

func newSnapshotReader(reader io.Reader, errChan chan error) *snapshotReader {
	return &snapshotReader{
		reader: reader,
		errCh:  errChan,
	}
}

var cmdCreate = &cli.Command{
	Name:  "create",
	Usage: "create a chain export",
	Description: TrimDescription(`
		Creating a snapshot can be configured in a few ways. The primary configuration is to use an epoch interval
		to calculate the appropiate epoch height.

		The epoch height is calculated by computing the current expected height, and finding the next interval that
		occurs after it, offset by the confidence. The current expected height is calculated using the current time,
		and the genesis timestamp.

		Eg: interval=100; confidence=15;

		            /- 500
		|----------|----------|----------|----------|
	           |----------|
		485 - /            \ - 585

		The calculation for the current expected height can be by-passed by using the 'after' flag. When set, the interval
		that occurs after the 'after' flag will be used for the epoch height.

		An exact epoch height can also be supplied with the 'height' flag.
	`),
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "name-prefix",
			Usage:   "add a prefix to the snapshot name",
			Value:   "default/",
			EnvVars: []string{"FCA_CREATE_NAME_PREFIX"},
		},
		&cli.StringFlag{
			Name:    "nodelocker-api",
			Usage:   "host and port of nodelocker api",
			Value:   "http://127.0.0.1:5100",
			EnvVars: []string{"FCA_CREATE_NODELOCKER_API"},
		},
		&cli.StringFlag{
			Name:    "bucket",
			Usage:   "bucket name for export upload",
			EnvVars: []string{"FCA_CREATE_BUCKET"},
		},
		&cli.StringFlag{
			Name:    "bucket-endpoint",
			Usage:   "bucket host and port for upload",
			EnvVars: []string{"FCA_CREATE_BUCKET_ENDPOINT"},
		},
		&cli.StringFlag{
			Name:    "retrieval-endpoint-prefix",
			Usage:   "URL prefix where uploaded object can be retrieved from",
			EnvVars: []string{"FCA_CREATE_RETRIEVAL_ENDPOINT_PREFIX"},
		},
		&cli.StringFlag{
			Name:    "access-key",
			Usage:   "access key for upload",
			EnvVars: []string{"FCA_CREATE_ACCESS_KEY"},
		},
		&cli.StringFlag{
			Name:    "secret-key",
			Usage:   "secret key for upload",
			EnvVars: []string{"FCA_CREATE_SECRET_KEY"},
		},
		&cli.BoolFlag{
			Name:    "discard",
			Usage:   "discard output, do not upload",
			EnvVars: []string{"FCA_CREATE_DISCARD"},
			Value:   false,
		},
		&cli.StringFlag{
			Name:    "config-path",
			Usage:   "path to configuration file",
			EnvVars: []string{"FCA_CONFIG_PATH"},
			Value:   "./config.toml",
		},
		&cli.IntFlag{
			Name:    "interval",
			Usage:   "interval used to determine next export height",
			EnvVars: []string{"FCA_CREATE_INTERVAL"},
			Value:   120,
		},
		&cli.IntFlag{
			Name:    "confidence",
			Usage:   "number of tipsets that should exist after the determine export height",
			EnvVars: []string{"FCA_CREATE_CONFIDENCE"},
			Value:   15,
		},
		&cli.IntFlag{
			Name:    "after",
			Usage:   "use interval height after this height",
			EnvVars: []string{"FCA_CREATE_AFTER"},
			Value:   0,
		},
		&cli.IntFlag{
			Name:    "height",
			Usage:   "create a snapshot from the given height",
			EnvVars: []string{"FCA_CREATE_HEIGHT"},
			Value:   0,
		},
		&cli.IntFlag{
			Name:    "stateroot-count",
			Usage:   "number of stateroots to included in snapshot",
			EnvVars: []string{"FCA_CREATE_STATEROOT_COUNT"},
			Value:   2000,
		},
		&cli.StringFlag{
			Name:    "filename",
			Usage:   "name of exported CAR file for internal chain export",
			EnvVars: []string{"FCA_EXPORT_FILENAME"},
		},
		&cli.DurationFlag{
			Name:    "progress-update",
			Usage:   "how frequenty to provide provide update logs",
			EnvVars: []string{"FCA_CREATE_PROGRESS_UPDATE"},
			Value:   60 * time.Second,
		},
		&cli.StringFlag{
			Name:    "export-dir",
			Usage:   "directory where to save the exported CAR file",
			EnvVars: []string{"FCA_EXPORT_DIR"},
		},
	},
	Action: func(cctx *cli.Context) error {
		ctx := context.Background()

		flagBucketEndpoint := cctx.String("bucket-endpoint")
		flagBucketAccessKey := cctx.String("access-key")
		flagBucketSecretKey := cctx.String("secret-key")
		flagNamePrefix := cctx.String("name-prefix")
		flagRetrievalEndpointPrefix := cctx.String("retrieval-endpoint-prefix")
		flagBucket := cctx.String("bucket")
		flagDiscard := cctx.Bool("discard")
		flagProgressUpdate := cctx.Duration("progress-update")
		flagNodeLockerAPI := cctx.String("nodelocker-api")
		flagConfigPath := cctx.String("config-path")
		flagInterval := cctx.Int("interval")
		flagConfidence := cctx.Int("confidence")
		flagAfter := cctx.Int("after")
		flagHeight := cctx.Int("height")
		flagStaterootCount := cctx.Int("stateroot-count")
		flagExportDir := cctx.String("export-dir")
		flagFileName := cctx.String("filename")

		u, err := url.Parse(flagBucketEndpoint)
		if err != nil {
			return err
		}

		icfg, err := config.FromFile(flagConfigPath, &config.ExportWorkerConfig{})
		if err != nil {
			return err
		}

		cfg := icfg.(*config.ExportWorkerConfig)

		addrs, err := NodeMultiaddrs(cfg)
		if err != nil {
			return err
		}

		var nodes []api.FullNode

		for _, addr := range addrs {
			node, closer, err := CreateLotusClient(ctx, addr)
			if err != nil {
				if errors.Is(err, syscall.ECONNREFUSED) {
					logger.Warnw("failed to dial node", "err", err)
				} else {
					logger.Warnw("failed to create node client", "err", err)
				}

				continue
			}

			defer closer()

			nodes = append(nodes, node)
		}

		if len(nodes) == 0 {
			return xerrors.Errorf("no nodes")
		}

		cm := consensus.NewConsensusManager(nodes)

		same, err := cm.CheckGenesis(ctx)
		if err != nil {
			return err
		}

		if !same {
			return xerrors.Errorf("nodes do not share the same genesis")
		}

		gtp, err := cm.GetGenesis(ctx)
		if err != nil {
			return err
		}

		now := time.Now()
		expected := export.GetExpectedHeightAt(gtp, now, 30*time.Second)

		var height abi.ChainEpoch
		if cctx.IsSet("height") {
			height = abi.ChainEpoch(flagHeight)
		} else {
			after := abi.ChainEpoch(flagAfter)
			if !cctx.IsSet("after") {
				after = expected
			}

			height = export.GetNextSnapshotHeight(after, abi.ChainEpoch(flagInterval), abi.ChainEpoch(flagConfidence), cctx.IsSet("after"))
		}

		confidenceHeight := height + abi.ChainEpoch(flagConfidence)

		t := export.TimeAtHeight(gtp, confidenceHeight, 30*time.Second)

		// Snapshots started
		logger.Infow("snapshot job started", "snapshot_height", height, "current_height", expected, "confidence_height", confidenceHeight, "run_at", t)
		time.Sleep(time.Until(t))
		bt := time.Now()

		headTs, err := cm.GetTipset(ctx, height)
		if err != nil {
			return err
		}

		tailHeight := flagHeight - flagStaterootCount
		tailTs, err := cm.GetTipset(ctx, abi.ChainEpoch(tailHeight))
		if err != nil {
			return err
		}

		nl, err := client.NewNodeLocker(ctx, flagNodeLockerAPI)
		if err != nil {
			return err
		}

		filterList, err := nl.LockedPeers(ctx)
		if err != nil {
			return err
		}

		var iteration int
		if cctx.IsSet("interval") {
			iteration = int(uint64(height)/uint64(flagInterval)) % len(nodes)
		} else {
			iteration = rand.Int() % len(nodes)
		}

		logger.Infow("iteration", "value", iteration)
		cm.ShiftStartNode(iteration)

		node, peerID, err := cm.GetNodeWithTipSet(ctx, headTs, filterList)
		if err != nil {
			return err
		}

		logger.Infow("node", "peer_id", peerID)

		lock, locked, err := nl.Lock(ctx, peerID)
		if err != nil {
			return err
		}

		if !locked {
			return xerrors.Errorf("failed to aquire lock")
		}

		e := export.NewExport(node, headTs, tailTs, flagFileName, flagExportDir)
		errCh := make(chan error)
		go func() {
			errCh <- e.Export(ctx)
		}()

		go func() {
			lock := lock
			for {
				select {
				case <-time.After(time.Until(lock.Expiry()) / 2):
					locked, err := lock.Renew(ctx)
					if err != nil {
						logger.Errorw("error updating lock", "err", err)
						continue
					}

					if !locked {
						logger.Errorw("failed to acquire lock")
						continue
					}

					logger.Debugw("lock aquired", "expiry", lock.Expiry())
				}
			}
		}()

		rrPath := filepath.Join(flagExportDir, flagFileName)
		for {
			info, err := os.Stat(rrPath)
			if os.IsNotExist(err) {
				logger.Infow("waiting for snapshot car file to begin writing")
				time.Sleep(time.Second * 15)
				continue
			} else if info.IsDir() {
				return xerrors.Errorf("trying to open directory instead of car file")
			}
			break
		}

		f, err := os.OpenFile(rrPath, os.O_RDONLY, 444)
		if err != nil {
			return err
		}
		defer f.Close()
		rr := newSnapshotReader(f, errCh)

		go func() {
			var lastSize int64
			for {
				select {
				case <-time.After(flagProgressUpdate):
					size := e.Progress(rrPath)
					if size == 0 {
						continue
					}
					logger.Infow("update", "total", size, "speed", (size-lastSize)/int64(flagProgressUpdate/time.Second))
					lastSize = size
				case err := <-errCh:
					if err != nil {
						break
					}
				}
			}
		}()

		if flagDiscard {
			logger.Infow("discarding output")
			g, ctxGroup := errgroup.WithContext(ctx)
			g.Go(func() error {
				return runWriteCompressed(ctxGroup, rrPath+".zstd", rr)
			})
			if err := g.Wait(); err != nil {
				return err
			}

			if err := <-errCh; err != nil {
				return err
			}
		} else {
			host := u.Hostname()
			port := u.Port()
			if port == "" {
				port = "80"
				if u.Scheme == "https" {
					port = "443"
				}
			}

			logger.Infow("upload endpoint", "host", host, "port", port, "tls", u.Scheme == "https")

			minioClient, err := minio.New(fmt.Sprintf("%s:%s", host, port), &minio.Options{
				Creds:  credentials.NewStaticV4(flagBucketAccessKey, flagBucketSecretKey, ""),
				Secure: u.Scheme == "https",
			})
			if err != nil {
				return err
			}

			//t := export.TimeAtHeight(gtp, height, 30*time.Second)

			logger.Infow("object", "name", flagFileName)

			g, ctxGroup := errgroup.WithContext(ctx)
			var siCompressed *snapshotInfo
			g.Go(func() error {
				var err error
				siCompressed, err = runUploadCompressed(ctxGroup, minioClient, flagBucket, flagNamePrefix, flagRetrievalEndpointPrefix, flagFileName+".zstd", peerID, bt, rr)
				return err
			})
			if err := g.Wait(); err != nil {
				return err
			}
			if err := <-errCh; err != nil {
				return err
			}

			sis := []*snapshotInfo{siCompressed}

			var sb strings.Builder
			for _, x := range sis {
				fmt.Fprintf(&sb, "%s *%s\n", x.digest, x.filename)
			}

			sha256sum := sb.String()

			_, err = minioClient.PutObject(ctx, flagBucket, fmt.Sprintf("%s%s.sha256sum", flagNamePrefix, flagFileName), strings.NewReader(sha256sum), -1, minio.PutObjectOptions{
				ContentDisposition: fmt.Sprintf("attachment; filename=\"%s.sha256sum\"", flagFileName),
				ContentType:        "text/plain",
			})
			if err != nil {
				logger.Errorw("failed to write sha256sum", "object", fmt.Sprintf("%s%s.sha256sum", flagNamePrefix, flagFileName), "err", err)
			}

			for _, x := range sis {
				info, err := minioClient.PutObject(ctx, flagBucket, fmt.Sprintf("%s%s", flagNamePrefix, x.latestIndex), strings.NewReader(x.latestLocation), -1, minio.PutObjectOptions{
					ContentType: "text/plain",
				})
				if err != nil {
					return fmt.Errorf("failed to write latest", "object", fmt.Sprintf("%slatest", flagNamePrefix), "err", err)
				}

				logger.Infow("latest upload",
					"bucket", info.Bucket,
					"key", info.Key,
					"etag", info.ETag,
					"size", info.Size,
					"location", info.Location,
					"version_id", info.VersionID,
					"expiration", info.Expiration,
					"expiration_rule_id", info.ExpirationRuleID,
				)
			}
		}

		logger.Infow("snapshot job finished", "elapsed", int64(time.Since(bt).Round(time.Second).Seconds()), "peer", peerID)

		return nil
	},
}

func compress(source io.Reader) io.Reader {
	r, w := io.Pipe()
	go func() {
		Compress(source, w)
		w.Close()
	}()
	return r
}

func runWriteCompressed(ctx context.Context, path string, source io.Reader) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	r := compress(source)
	n, err := io.Copy(file, r)
	if err != nil {
		return err
	}
	logger.Infow("data copied to file:", n)
	return nil
}

func runUploadCompressed(ctx context.Context, minioClient *minio.Client, flagBucket, flagNamePrefix, flagRetrievalEndpointPrefix, name, peerID string, bt time.Time, source io.Reader) (*snapshotInfo, error) {
	r1 := compress(source)

	h := sha256.New()
	r := io.TeeReader(r1, h)

	filename := name

	info, err := minioClient.PutObject(ctx, flagBucket, fmt.Sprintf("%s%s", flagNamePrefix, filename), r, -1, minio.PutObjectOptions{
		ContentDisposition: fmt.Sprintf("attachment; filename=\"%s\"", filename),
		ContentType:        "application/octet-stream",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to upload object (%s): %w", fmt.Sprintf("%s%s", flagNamePrefix, filename), err)
	}

	logger.Infow("compressed snapshot upload",
		"bucket", info.Bucket,
		"key", info.Key,
		"etag", info.ETag,
		"size", info.Size,
		"location", info.Location,
		"version_id", info.VersionID,
		"expiration", info.Expiration,
		"expiration_rule_id", info.ExpirationRuleID,
	)

	snapshotSize := info.Size

	latestLocation, err := url.JoinPath(flagRetrievalEndpointPrefix, info.Key)
	if err != nil {
		logger.Errorw("failed to join request path", "request_prefix", flagRetrievalEndpointPrefix, "key", info.Key)
		return nil, fmt.Errorf("failed to join request path: %w", err)
	}

	digest := fmt.Sprintf("%x", h.Sum(nil))

	logger.Infow("compressed snapshot job finished", "digiest", digest, "elapsed", int64(time.Since(bt).Round(time.Second).Seconds()), "size", snapshotSize, "peer", peerID)

	return &snapshotInfo{
		digest:         digest,
		size:           snapshotSize,
		filename:       filename,
		latestIndex:    "latest.zst",
		latestLocation: latestLocation,
	}, nil
}
