package slimproto.server.android

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log
import androidx.core.content.ContextCompat

/**
 * Optionally auto-starts the SlimProto server after device boot.
 * Only runs when the user has enabled "Auto-start on boot" in the app settings.
 */
class BootReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED) return

        val prefs = context.getSharedPreferences(MainActivity.PREFS_NAME, Context.MODE_PRIVATE)
        if (!prefs.getBoolean(MainActivity.PREF_AUTO_START, false)) return

        val serviceIntent = Intent(context, ServerService::class.java)
        ContextCompat.startForegroundService(context, serviceIntent)
        Log.d(TAG, "Auto-starting SlimProto server after boot")
    }

    companion object {
        private const val TAG = "BootReceiver"
    }
}
