from pathlib import Path

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", env_file_encoding="utf-8", extra="ignore")

    postgres_host: str = "localhost"
    postgres_port: int = 5432
    postgres_db: str = "ontology"
    postgres_user: str = "ontology"
    postgres_password: str = "ontology"

    neo4j_uri: str = "bolt://localhost:7687"
    neo4j_user: str = "neo4j"
    neo4j_password: str = "ontology-dev"

    mesh_year: int = 2025

    prescriber_year: int = 2023
    prescriber_state: str = "CA"
    prescriber_url: str = (
        "https://data.cms.gov/sites/default/files/2025-04/"
        "0d5915ce-002c-4d87-bde8-24ffb08bb6cc/"
        "MUP_DPR_RY25_P04_V10_DY23_NPIBN.csv"
    )

    data_dir: Path = Path("./data")

    @property
    def postgres_dsn(self) -> str:
        return (
            f"postgresql://{self.postgres_user}:{self.postgres_password}"
            f"@{self.postgres_host}:{self.postgres_port}/{self.postgres_db}"
        )

    @property
    def raw_dir(self) -> Path:
        return self.data_dir / "raw"


settings = Settings()
