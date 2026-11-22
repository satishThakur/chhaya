FROM python:3.11-slim
RUN useradd --uid 10002 --create-home sync
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY garmin_sync/ ./garmin_sync/
USER sync
CMD ["python", "-m", "garmin_sync.main"]
