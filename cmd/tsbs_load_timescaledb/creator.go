package main

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"

	"github.com/jmoiron/sqlx"
)

type dbCreator struct {
	tags    string
	cols    string
	connStr string
}

func (d *dbCreator) Init() {
	br := loader.GetBufferedReader()
	d.readDataHeader(br)

	// Needed to connect to user's database in order to drop/create db-name database
	re := regexp.MustCompile(`(dbname)=\S*\b`)
	d.connStr = re.ReplaceAllString(getConnectString(), "")
}

func (d *dbCreator) readDataHeader(br *bufio.Reader) {
	// First three lines are header, with the first line containing the tags
	// and their names, the second line containing the column names, and
	// third line being blank to separate from the data
	for i := 0; i < 3; i++ {
		var err error
		var empty string
		if i == 0 {
			d.tags, err = br.ReadString('\n')
			d.tags = strings.TrimSpace(d.tags)
		} else if i == 1 {
			d.cols, err = br.ReadString('\n')
			d.cols = strings.TrimSpace(d.cols)
		} else {
			empty, err = br.ReadString('\n')
			empty = strings.TrimSpace(empty)
			if len(empty) > 0 {
				fatal("input has wrong header format: third line is not blank")
			}
		}
		if err != nil {
			fatal("input has wrong header format: %v", err)
		}
	}
}

func (d *dbCreator) DBExists(dbName string) bool {
	db := sqlx.MustConnect(dbType, d.connStr)
	defer db.Close()
	r, _ := db.Queryx("SELECT 1 from pg_database WHERE datname = $1", dbName)
	defer r.Close()
	return r.Next()
}

func (d *dbCreator) RemoveOldDB(dbName string) error {
	db := sqlx.MustConnect(dbType, d.connStr)
	defer db.Close()
	db.MustExec("DROP DATABASE IF EXISTS " + dbName)
	return nil
}

func (d *dbCreator) CreateDB(dbName string) error {
	db := sqlx.MustConnect(dbType, d.connStr)
	db.MustExec("CREATE DATABASE " + dbName)
	db.Close()

	dbBench := sqlx.MustConnect(dbType, getConnectString())
	defer dbBench.Close()

	parts := strings.Split(strings.TrimSpace(d.tags), ",")
	if parts[0] != "tags" {
		return fmt.Errorf("input header in wrong format. got '%s', expected 'tags'", parts[0])
	}
	createTagsTable(dbBench, parts[1:])
	tableCols["tags"] = parts[1:]

	parts = strings.Split(strings.TrimSpace(d.cols), ",")
	hypertable := parts[0]
	partitioningField := tableCols["tags"][0]
	tableCols[hypertable] = parts[1:]

	psuedoCols := []string{}
	if inTableTag {
		psuedoCols = append(psuedoCols, partitioningField)
	}

	fieldDef := []string{}
	indexes := []string{}
	psuedoCols = append(psuedoCols, parts[1:]...)
	extraCols := 0 // set to 1 when hostname is kept in-table
	for idx, field := range psuedoCols {
		if len(field) == 0 {
			continue
		}
		fieldType := "DOUBLE PRECISION"
		idxType := fieldIndex
		if inTableTag && idx == 0 {
			fieldType = "TEXT"
			idxType = ""
			extraCols = 1
		}

		fieldDef = append(fieldDef, fmt.Sprintf("%s %s", field, fieldType))
		if fieldIndexCount == -1 || idx < (fieldIndexCount+extraCols) {
			indexes = append(indexes, d.getCreateIndexOnFieldCmds(hypertable, field, idxType)...)
		}
	}
	dbBench.MustExec(fmt.Sprintf("CREATE TABLE %s (time timestamptz, tags_id integer, %s)", hypertable, strings.Join(fieldDef, ",")))
	if partitionIndex {
		dbBench.MustExec(fmt.Sprintf("CREATE INDEX ON %s(tags_id, \"time\" DESC)", hypertable))
	}

	// Only allow one or the other, it's probably never right to have both.
	// Experimentation suggests (so far) that for 100k devices it is better to
	// use --time-partition-index for reduced index lock contention.
	if timePartitionIndex {
		dbBench.MustExec(fmt.Sprintf("CREATE INDEX ON %s(\"time\" DESC, tags_id)", hypertable))
	} else if timeIndex {
		dbBench.MustExec(fmt.Sprintf("CREATE INDEX ON %s(\"time\" DESC)", hypertable))
	}

	for _, idxDef := range indexes {
		dbBench.MustExec(idxDef)
	}

	if useHypertable {
		dbBench.MustExec("CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE")
		dbBench.MustExec(
			fmt.Sprintf("SELECT create_hypertable('%s'::regclass, 'time'::name, partitioning_column => '%s'::name, number_partitions => %v::smallint, chunk_time_interval => %d, create_default_indexes=>FALSE)",
				hypertable, "tags_id", numberPartitions, chunkTime.Nanoseconds()/1000))
	}

	return nil
}

func (d *dbCreator) getCreateIndexOnFieldCmds(hypertable, field, idxType string) []string {
	ret := []string{}
	for _, idx := range strings.Split(idxType, ",") {
		if idx == "" {
			continue
		}

		indexDef := ""
		if idx == timeValueIdx {
			indexDef = fmt.Sprintf("(time DESC, %s)", field)
		} else if idx == valueTimeIdx {
			indexDef = fmt.Sprintf("(%s, time DESC)", field)
		} else {
			fatal("Unknown index type %v", idx)
		}

		ret = append(ret, fmt.Sprintf("CREATE INDEX ON %s %s", hypertable, indexDef))
	}
	return ret
}