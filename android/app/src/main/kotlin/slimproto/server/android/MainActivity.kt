package slimproto.server.android

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.widget.Button
import android.widget.Switch
import android.widget.TextView
import androidx.activity.ComponentActivity
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat

class MainActivity : ComponentActivity() {

    companion object {
        const val PREFS_NAME = "slimproto_prefs"
        const val PREF_AUTO_START = "auto_start"
        private const val REQ_NOTIF = 1001
    }

    private lateinit var statusText: TextView
    private lateinit var toggleButton: Button
    private lateinit var bootSwitch: Switch

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)

        statusText = findViewById(R.id.status_text)
        toggleButton = findViewById(R.id.toggle_button)
        bootSwitch = findViewById(R.id.boot_switch)

        bootSwitch.setOnCheckedChangeListener { _, checked ->
            getSharedPreferences(PREFS_NAME, MODE_PRIVATE).edit()
                .putBoolean(PREF_AUTO_START, checked)
                .apply()
        }

        toggleButton.setOnClickListener {
            if (ServerService.isRunning) {
                stopServer()
            } else {
                startServer()
            }
        }
    }

    override fun onResume() {
        super.onResume()
        refreshUi()
    }

    private fun refreshUi() {
        val prefs = getSharedPreferences(PREFS_NAME, MODE_PRIVATE)
        bootSwitch.isChecked = prefs.getBoolean(PREF_AUTO_START, false)

        val running = ServerService.isRunning
        toggleButton.setText(if (running) R.string.stop else R.string.start)
        statusText.text = getString(
            if (running) R.string.server_running else R.string.server_stopped
        )
    }

    private fun startServer() {
        // Foreground-service notifications require a runtime permission on Android 13+.
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU
            && ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS)
            != PackageManager.PERMISSION_GRANTED
        ) {
            ActivityCompat.requestPermissions(
                this,
                arrayOf(Manifest.permission.POST_NOTIFICATIONS),
                REQ_NOTIF
            )
            return
        }
        ContextCompat.startForegroundService(this, Intent(this, ServerService::class.java))
        statusText.text = getString(R.string.server_starting)
    }

    private fun stopServer() {
        val intent = Intent(this, ServerService::class.java).apply {
            action = ServerService.ACTION_STOP
        }
        startService(intent)
        statusText.text = getString(R.string.server_stopping)
    }

    override fun onRequestPermissionsResult(
        requestCode: Int,
        permissions: Array<out String>,
        grantResults: IntArray
    ) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        if (requestCode == REQ_NOTIF
            && grantResults.firstOrNull() == PackageManager.PERMISSION_GRANTED
        ) {
            startServer()
        }
    }
}
