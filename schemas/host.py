from typing import Literal
import base


class Host(base.Base):
    kind: Literal["host"]
    hostName: str
