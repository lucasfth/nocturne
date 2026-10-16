"""Bound incomplete request headers, including slow drip and idle connections.
Uses uvicorn H11 hooks; verify on uvicorn upgrades. Body deadlines are in edge.py.
"""
import h11
from uvicorn.protocols.http.h11_impl import H11Protocol


class EdgeH11Protocol(H11Protocol):
    header_deadline_s=10
    def arm(self):
        if getattr(self,'header_timer',None): self.header_timer.cancel()
        self.header_timer=self.loop.call_later(self.header_deadline_s,self.transport.close)
    def connection_made(self,transport):
        super().connection_made(transport)
        self.arm()
    def data_received(self,data):
        # A new request on an idle keepalive connection gets an absolute deadline.
        if self.conn.their_state is h11.IDLE and not getattr(self,'header_timer',None): self.arm()
        super().data_received(data)
        if self.conn.their_state is not h11.IDLE and getattr(self,'header_timer',None):
            self.header_timer.cancel(); self.header_timer=None
    def connection_lost(self,exc):
        if getattr(self,'header_timer',None): self.header_timer.cancel(); self.header_timer=None
        super().connection_lost(exc)
