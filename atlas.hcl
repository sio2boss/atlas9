// Atlas configuration file
// See: https://atlasgo.io/atlas-schema/projects

env "local" {
  src = "file://schema.sql"
  url = "postgres://postgres:postgres@localhost:5432/mydb?sslmode=disable"
  dev = "docker://postgres/15"
}

env "prod" {
  src = "file://schema.sql"
  url = env("DATABASE_URL_PROD")
}
