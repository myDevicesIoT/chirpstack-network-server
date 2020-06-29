package storage

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/gob"
	"fmt"
	"strconv"
	"time"

	"github.com/go-redis/redis/v7"
	"github.com/gofrs/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/brocaar/chirpstack-network-server/internal/logging"
	"github.com/brocaar/lorawan"
)

// template used for generating Redis keys
const (
	gatewayKeyTempl = "lora:ns:gw:%s"
)

// GPSPoint contains a GPS point.
type GPSPoint struct {
	Latitude  float64
	Longitude float64
}

// Value implements the driver.Valuer interface.
func (l GPSPoint) Value() (driver.Value, error) {
	return fmt.Sprintf("(%s,%s)", strconv.FormatFloat(l.Latitude, 'f', -1, 64), strconv.FormatFloat(l.Longitude, 'f', -1, 64)), nil
}

// Scan implements the sql.Scanner interface.
func (l *GPSPoint) Scan(src interface{}) error {
	b, ok := src.([]byte)
	if !ok {
		return fmt.Errorf("expected []byte, got %T", src)
	}

	_, err := fmt.Sscanf(string(b), "(%f,%f)", &l.Latitude, &l.Longitude)
	return err
}

// Gateway represents a gateway.
type Gateway struct {
	GatewayID        lorawan.EUI64  `db:"gateway_id"`
	RoutingProfileID uuid.UUID      `db:"routing_profile_id"`
	CreatedAt        time.Time      `db:"created_at"`
	UpdatedAt        time.Time      `db:"updated_at"`
	FirstSeenAt      *time.Time     `db:"first_seen_at"`
	LastSeenAt       *time.Time     `db:"last_seen_at"`
	Location         GPSPoint       `db:"location"`
	Altitude         float64        `db:"altitude"`
	TLSCert          []byte         `db:"tls_cert"`
	GatewayProfileID *uuid.UUID     `db:"gateway_profile_id"`
	Boards           []GatewayBoard `db:"-"`
}

// GatewayBoard holds the gateway board configuration.
type GatewayBoard struct {
	FPGAID           *lorawan.EUI64     `db:"fpga_id"`
	FineTimestampKey *lorawan.AES128Key `db:"fine_timestamp_key"`
}

// CreateGateway creates the given gateway.
func CreateGateway(ctx context.Context, db sqlx.Execer, gw *Gateway) error {
	now := time.Now()
	gw.CreatedAt = now
	gw.UpdatedAt = now

	_, err := db.Exec(`
		insert into gateway (
			gateway_id,
			created_at,
			updated_at,
			first_seen_at,
			last_seen_at,
			location,
			altitude,
			gateway_profile_id,
			routing_profile_id,
			tls_cert
		) values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		gw.GatewayID[:],
		gw.CreatedAt,
		gw.UpdatedAt,
		gw.FirstSeenAt,
		gw.LastSeenAt,
		gw.Location,
		gw.Altitude,
		gw.GatewayProfileID,
		gw.RoutingProfileID,
		gw.TLSCert,
	)
	if err != nil {
		return handlePSQLError(err, "insert error")
	}

	for i, board := range gw.Boards {
		_, err := db.Exec(`
			insert into gateway_board (
				id,
				gateway_id,
				fpga_id,
				fine_timestamp_key
			) values ($1, $2, $3, $4)`,
			i,
			gw.GatewayID,
			board.FPGAID,
			board.FineTimestampKey,
		)
		if err != nil {
			return handlePSQLError(err, "insert error")
		}
	}

	log.WithFields(log.Fields{
		"gateway_id": gw.GatewayID,
		"ctx_id":     ctx.Value(logging.ContextIDKey),
	}).Info("gateway created")
	return nil
}

// CreateGatewayCache caches the given gateway in Redis.
// The TTL of the gateway is the same as that of the device-sessions.
func CreateGatewayCache(ctx context.Context, gw Gateway) error {
	key := fmt.Sprintf(gatewayKeyTempl, gw.GatewayID)

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(gw); err != nil {
		return errors.Wrap(err, "gob encode gateway error")
	}

	err := RedisClient().Set(key, buf.Bytes(), deviceSessionTTL).Err()
	if err != nil {
		return errors.Wrap(err, "set gateway error")
	}

	return nil
}

// GetGatewayCache returns a cached gateway.
func GetGatewayCache(ctx context.Context, gatewayID lorawan.EUI64) (Gateway, error) {
	var gw Gateway
	key := fmt.Sprintf(gatewayKeyTempl, gatewayID)

	val, err := RedisClient().Get(key).Bytes()
	if err != nil {
		if err == redis.Nil {
			return gw, ErrDoesNotExist
		}
		return gw, errors.Wrap(err, "get error")
	}

	err = gob.NewDecoder(bytes.NewReader(val)).Decode(&gw)
	if err != nil {
		return gw, errors.Wrap(err, "gob decode error")
	}

	return gw, nil
}

// FlushGatewayCache deletes a cached gateway.
func FlushGatewayCache(ctx context.Context, gatewayID lorawan.EUI64) error {
	key := fmt.Sprintf(gatewayKeyTempl, gatewayID)

	err := RedisClient().Del(key).Err()
	if err != nil {
		return errors.Wrap(err, "delete error")
	}

	return nil
}

// GetAndCacheGateway returns a gateway from the cache in case it is available.
// In case the gateway is not cached, it will be retrieved from the database
// and then cached.
func GetAndCacheGateway(ctx context.Context, db sqlx.Queryer, gatewayID lorawan.EUI64) (Gateway, error) {
	gw, err := GetGatewayCache(ctx, gatewayID)
	if err == nil {
		return gw, nil
	}

	if err != ErrDoesNotExist {
		log.WithFields(log.Fields{
			"ctx_id":     ctx.Value(logging.ContextIDKey),
			"gateway_id": gatewayID,
		}).WithError(err).Error("get gateway cache error")
		// we don't return the error as we can still fall-back onto db retrieval
	}

	gw, err = GetGateway(ctx, db, gatewayID)
	if err != nil {
		return gw, errors.Wrap(err, "get gateway error")
	}

	err = CreateGatewayCache(ctx, gw)
	if err != nil {
		log.WithFields(log.Fields{
			"ctx_id":     ctx.Value(logging.ContextIDKey),
			"gateway_id": gatewayID,
		}).WithError(err).Error("create gateway cache error")
	}

	return gw, nil
}

// GetGateway returns the gateway for the given Gateway ID.
func GetGateway(ctx context.Context, db sqlx.Queryer, id lorawan.EUI64) (Gateway, error) {
	var gw Gateway
	err := sqlx.Get(db, &gw, "select * from gateway where gateway_id = $1", id[:])
	if err != nil {
		return gw, handlePSQLError(err, "select error")
	}

	err = sqlx.Select(db, &gw.Boards, `
		select
			fpga_id,
			fine_timestamp_key
		from
			gateway_board
		where
			gateway_id = $1
		order by
			id
		`,
		id,
	)
	if err != nil {
		return gw, handlePSQLError(err, "select error")
	}

	return gw, nil
}

// UpdateGateway updates the given gateway.
func UpdateGateway(ctx context.Context, db sqlx.Execer, gw *Gateway) error {
	now := time.Now()
	gw.UpdatedAt = now

	res, err := db.Exec(`
		update gateway set
			updated_at = $2,
			first_seen_at = $3,
			last_seen_at = $4,
			location = $5,
			altitude = $6,
			gateway_profile_id = $7,
			routing_profile_id = $8,
			tls_cert = $9
		where gateway_id = $1`,
		gw.GatewayID[:],
		gw.UpdatedAt,
		gw.FirstSeenAt,
		gw.LastSeenAt,
		gw.Location,
		gw.Altitude,
		gw.GatewayProfileID,
		gw.RoutingProfileID,
		gw.TLSCert,
	)
	if err != nil {
		return handlePSQLError(err, "update error")
	}
	ra, err := res.RowsAffected()
	if err != nil {
		return errors.Wrap(err, "get rows affected error")
	}
	if ra == 0 {
		return ErrDoesNotExist
	}

	_, err = db.Exec(`
		delete from gateway_board where gateway_id = $1`,
		gw.GatewayID,
	)
	if err != nil {
		return handlePSQLError(err, "delete error")
	}

	for i, board := range gw.Boards {
		_, err := db.Exec(`
			insert into gateway_board (
				id,
				gateway_id,
				fpga_id,
				fine_timestamp_key
			) values ($1, $2, $3, $4)`,
			i,
			gw.GatewayID,
			board.FPGAID,
			board.FineTimestampKey,
		)
		if err != nil {
			return handlePSQLError(err, "insert error")
		}
	}

	log.WithFields(log.Fields{
		"gateway_id": gw.GatewayID,
		"ctx_id":     ctx.Value(logging.ContextIDKey),
	}).Info("gateway updated")
	return nil
}

// DeleteGateway deletes the gateway matching the given Gateway ID.
func DeleteGateway(ctx context.Context, db sqlx.Execer, id lorawan.EUI64) error {
	res, err := db.Exec("delete from gateway where gateway_id = $1", id[:])
	if err != nil {
		return handlePSQLError(err, "delete error")
	}
	ra, err := res.RowsAffected()
	if err != nil {
		return errors.Wrap(err, "get rows affected error")
	}
	if ra == 0 {
		return ErrDoesNotExist
	}
	log.WithFields(log.Fields{
		"gateway_id": id,
		"ctx_id":     ctx.Value(logging.ContextIDKey),
	}).Info("gateway deleted")
	return nil
}

// GetGatewaysForIDs returns a map of gateways given a slice of IDs.
func GetGatewaysForIDs(ctx context.Context, db sqlx.Queryer, ids []lorawan.EUI64) (map[lorawan.EUI64]Gateway, error) {
	out := make(map[lorawan.EUI64]Gateway)
	var idsB [][]byte
	for i := range ids {
		idsB = append(idsB, ids[i][:])
	}

	var gws []Gateway
	err := sqlx.Select(db, &gws, "select * from gateway where gateway_id = any($1)", pq.ByteaArray(idsB))
	if err != nil {
		return nil, handlePSQLError(err, "select error")
	}

	if len(gws) != len(ids) {
		return nil, fmt.Errorf("expected %d gateways, got %d", len(ids), len(out))
	}

	for i := range gws {
		out[gws[i].GatewayID] = gws[i]
	}

	return out, nil
}
