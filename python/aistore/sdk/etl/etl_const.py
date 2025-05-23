# Defaults
DEFAULT_ETL_COMM = "hpush"
DEFAULT_ETL_TIMEOUT = "5m"
DEFAULT_ETL_RUNTIME = "python3.13v2"

# ETL comm types
# ext/etl/api.go Hpush
ETL_COMM_HPUSH = "hpush"
# ext/etl/api.go Hpull
ETL_COMM_HPULL = "hpull"
# ext/etl/api.go WebSocket
ETL_COMM_WS = "ws"
# ext/etl/api.go HpushStdin
ETL_COMM_IO = "io"

# ETL lifecycle stages (see docs/etl.md#etl-pod-lifecycle)
ETL_STAGE_INIT = "Initializing"
ETL_STAGE_RUNNING = "Running"
ETL_STAGE_STOPPED = "Stopped"

ETL_COMM_CODE = [ETL_COMM_IO, ETL_COMM_HPUSH, ETL_COMM_HPULL]
ETL_COMM_SPEC = [ETL_COMM_HPUSH, ETL_COMM_HPULL, ETL_COMM_WS]

ETL_SUPPORTED_PYTHON_VERSIONS = ["3.9", "3.10", "3.11", "3.12", "3.13"]

# templates for ETL

CODE_TEMPLATE = """
import pickle
import base64
import importlib

for mod in {}:
    importlib.import_module(mod)
    
transform = pickle.loads(base64.b64decode('{}'))
{}
"""
