// M1 schema bootstrap.
//
// Defines the canonical Episode (raw event from the log) and Entity
// (canonical record after resolution) node tables, plus the MENTIONS
// provenance edge and the RELATES_TO typed-relation edge between
// entities.
//
// See agent-docs/tech/ladybug.md (Schema sketch).
// Every statement is `IF NOT EXISTS` so the file is safe to replay if a
// previous apply crashed between statements (the runner records the version
// row only after every statement succeeds).
CREATE NODE TABLE IF NOT EXISTS Entity(
    uid STRING,
    kind STRING,
    displayName STRING,
    confidence DOUBLE,
    resolutionStatus STRING,
    createTime TIMESTAMP,
    PRIMARY KEY (uid)
);

CREATE NODE TABLE IF NOT EXISTS Episode(
    uid STRING,
    createTime TIMESTAMP,
    kind STRING,
    source STRING,
    confidence DOUBLE,
    PRIMARY KEY (uid)
);

CREATE REL TABLE IF NOT EXISTS MENTIONS(FROM Episode TO Entity);

CREATE REL TABLE IF NOT EXISTS RELATES_TO(
    FROM Entity TO Entity,
    predicate STRING,
    confidence DOUBLE
);
