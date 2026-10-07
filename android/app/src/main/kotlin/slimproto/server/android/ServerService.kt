package slimproto.server.android

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Intent
import android.os.Build
import android.os.IBinder
import android.util.Log
import androidx.core.app.NotificationCompat
import java.io.BufferedReader
import java.io.IOException
import java.io.InputStreamReader
import java.util.concurrent.TimeUnit

/**
 * Runs the Go SlimProto server as a foreground service.
 *
 * The bundled `server_android_arm64` binary is extracted into the app's private
 * files directory, marked executable, and launched with its working directory set
 * there (so it finds `presets.json`). The server binds to 127.0.0.1 on ports
 * 3483 (SlimProto), 9000 (CometD/HTTP) and 9090 (CLI), so player apps on the
 * same device can connect via localhost.
 */
class ServerService : Service() {

    companion object {
        private const val TAG = "SlimProtoService"
        const val CHANNEL_ID = "slimproto_server"
        private const val NOTIFICATION_ID = 1
        const val ACTION_STOP = "slimproto.server.android.action.STOP"

        private const val BINARY_ASSET = "server_android_arm64"
        private const val PRESETS_ASSET = "presets.json"

        @Volatile
        var isRunning = false
            private set
    }

    private var process: Process? = null
    private var logThread: Thread? = null

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        createNotificationChannel()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopSelf()
            return START_NOT_STICKY
        }
        startForeground(NOTIFICATION_ID, buildNotification())
        if (!isRunning) {
            isRunning = true
            startServer()
        }
        return START_STICKY
    }

    override fun onDestroy() {
        stopServer()
        isRunning = false
        super.onDestroy()
    }

    private fun createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val manager = getSystemService(NotificationManager::class.java)
            manager.createNotificationChannel(
                NotificationChannel(
                    CHANNEL_ID,
                    getString(R.string.channel_name),
                    NotificationManager.IMPORTANCE_LOW
                )
            )
        }
    }

    private fun buildNotification(): Notification {
        val stopIntent = Intent(this, ServerService::class.java).apply { action = ACTION_STOP }
        val pending = PendingIntent.getService(
            this, 0, stopIntent, PendingIntent.FLAG_IMMUTABLE
        )
        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setContentTitle(getString(R.string.app_name))
            .setContentText(getString(R.string.server_running))
            .setSmallIcon(R.drawable.ic_launcher)
            .setOngoing(true)
            .addAction(
                NotificationCompat.Action.Builder(
                    R.drawable.ic_launcher, getString(R.string.stop), pending
                ).build()
            )
            .build()
    }

    private fun startServer() {
        try {
            val binary = BinaryExtractor.extractAssetOverwrite(this, BINARY_ASSET)
            // presets.json is read relative to the working directory; keep user edits.
            BinaryExtractor.extractAsset(this, PRESETS_ASSET)

            val pb = ProcessBuilder(binary.absolutePath)
            pb.directory(filesDir)
            pb.redirectErrorStream(true)
            process = pb.start()

            logThread = Thread({
                val stream = process?.inputStream ?: return@Thread
                val reader = BufferedReader(InputStreamReader(stream))
                while (isRunning) {
                    val line = try {
                        reader.readLine()
                    } catch (e: IOException) {
                        null
                    }
                    if (line == null) break
                    Log.d(TAG, line)
                }
            }, "slimproto-log").apply { isDaemon = true }
            logThread?.start()

            Log.i(TAG, "SlimProto server started: ${binary.absolutePath}")
        } catch (e: Exception) {
            isRunning = false
            Log.e(TAG, "Failed to start SlimProto server", e)
        }
    }

    private fun stopServer() {
        val p = process ?: return
        try {
            // destroy() sends SIGTERM; the Go server handles it for a graceful shutdown.
            p.destroy()
            try {
                p.waitFor(3, TimeUnit.SECONDS)
            } catch (e: InterruptedException) {
                Thread.currentThread().interrupt()
            }
            if (p.isAlive) {
                p.destroyForcibly()
            }
        } catch (e: Exception) {
            Log.w(TAG, "Error stopping server", e)
        } finally {
            process = null
            logThread = null
            Log.i(TAG, "SlimProto server stopped")
        }
    }
}
