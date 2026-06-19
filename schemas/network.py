from base import Base
from pydantic import IPvAnyNetwork


class Network(Base):
    kind: str = "network"
    name: str
    address: IPvAnyNetwork
