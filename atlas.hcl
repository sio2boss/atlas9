// Atlas configuration file
// See: https://atlasgo.io/atlas-schema/projects
// Database URL for both environments: APP_DB_URL

env "localdev" {
  src = "file://schema.sql"
  url = getenv("APP_DB_URL")
  dev = "docker://postgres/15"
}


env "dev" {
  src = "file://schema.sql"
  url = getenv("APP_DB_URL")
}


env "prod" {
  src = "file://schema.sql"
  url = getenv("APP_DB_URL")
}
