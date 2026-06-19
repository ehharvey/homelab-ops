import pydantic
from abc import ABC, abstractmethod


class Base(pydantic.BaseModel, ABC):
    kind: str
    id: str


class Metadata(Base):
    name: str
    description: str = ""
