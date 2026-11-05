// Package store persists what the server knows about sessions and the
// credentials it has minted. The Store interface is the schema; a memory
// implementation serves local mode and a Postgres implementation serves
// hosted mode.
//
// @decision(DL-001)
package store
