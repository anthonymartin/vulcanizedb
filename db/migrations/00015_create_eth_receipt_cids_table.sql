-- +goose Up
CREATE TABLE eth.receipt_cids (
  id                    SERIAL PRIMARY KEY,
  tx_id                 INTEGER NOT NULL REFERENCES eth.transaction_cids (id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
  cid                   TEXT NOT NULL,
  contract              VARCHAR(66),
  topic0s               VARCHAR(66)[],
  topic1s               VARCHAR(66)[],
  topic2s               VARCHAR(66)[],
  topic3s               VARCHAR(66)[]
);

-- +goose Down
DROP TABLE eth.receipt_cids;