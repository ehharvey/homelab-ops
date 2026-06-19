from typing import List, Optional

from pydantic import field_validator
from cryptography.hazmat.primitives import serialization
from base import Base


class UserSsh(Base):
    publicKeys: List[str]

    @field_validator("publicKeys")
    def validate_public_keys(cls, v):
        exceptions = []
        for key in v:
            try:
                serialization.load_ssh_public_key(key.encode())
            except Exception as e:
                exceptions.append(e)
        if exceptions:
            raise ValueError(f"Invalid public keys: {exceptions}")
        else:
            return v


class User(Base):
    username: str
    password: Optional[str] = None
    ssh: UserSsh
