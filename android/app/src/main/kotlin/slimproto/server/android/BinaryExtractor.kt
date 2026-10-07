package slimproto.server.android

import android.content.Context
import java.io.File
import java.io.FileOutputStream
import java.io.IOException

object BinaryExtractor {

    /**
     * Copies [assetName] from assets into the app's private files directory,
     * but only if it does not already exist (preserving user edits such as presets.json).
     * Ensures the file is executable.
     */
    fun extractAsset(context: Context, assetName: String): File {
        val outFile = File(context.filesDir, assetName)
        if (!outFile.exists()) {
            copyAsset(context, assetName, outFile)
        }
        outFile.setExecutable(true, false)
        return outFile
    }

    /**
     * Copies [assetName] from assets into the app's private files directory,
     * always overwriting. Used for the Go server binary so app updates ship a fresh binary.
     * Ensures the file is executable.
     */
    fun extractAssetOverwrite(context: Context, assetName: String): File {
        val outFile = File(context.filesDir, assetName)
        copyAsset(context, assetName, outFile)
        outFile.setExecutable(true, false)
        return outFile
    }

    @Throws(IOException::class)
    private fun copyAsset(context: Context, assetName: String, outFile: File) {
        context.assets.open(assetName).use { input ->
            FileOutputStream(outFile).use { output ->
                input.copyTo(output)
            }
        }
    }
}
